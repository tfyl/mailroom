#!/bin/sh
# Reports whether mailroom's redirect URIs are registered on a Google OAuth client.
#
# Terraform cannot create that client, so it cannot guarantee it is right either — but it can
# ask. Google's authorization endpoint answers a well-formed request with a redirect: to the
# sign-in page when the URI is registered, and to /signin/oauth/error when it is not, carrying
# an authError payload whose leading field is the error code.
#
# Read by data.external, so the contract is strict: a single JSON object of strings on stdout,
# and exit 0 whatever happens. Exiting non-zero would break `terraform plan` for somebody who
# is merely offline, which is a worse outcome than not knowing.
set -eu

INPUT=$(cat)

field() {
  printf '%s' "$INPUT" | sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

CLIENT_ID=$(field client_id)
PUBLIC_URL=$(field public_url)
PUBLIC_URL=${PUBLIC_URL%/}

# urlencode keeps this dependency-free: no python, no jq, just the shell and curl.
urlencode() {
  printf '%s' "$1" | sed -e 's|:|%3A|g' -e 's|/|%2F|g' -e 's|?|%3F|g' -e 's|&|%26|g' -e 's|=|%3D|g'
}

probe() {
  uri="$1"
  if [ -z "$CLIENT_ID" ]; then
    printf 'unknown'
    return
  fi

  location=$(curl -s -o /dev/null -D - --max-time 15 \
    "https://accounts.google.com/o/oauth2/v2/auth?client_id=$(urlencode "$CLIENT_ID")&redirect_uri=$(urlencode "$uri")&response_type=code&scope=openid" \
    2>/dev/null | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')

  case "$location" in
    '')                        printf 'unknown' ;;
    *"/signin/oauth/error"*)
      # The payload is base64 of a length-prefixed field; the code is legible in the decode.
      code=$(printf '%s' "$location" | sed -n 's/.*authError=\([^&]*\).*/\1/p' \
        | tr '_-' '/+' | base64 -d 2>/dev/null | tr -dc '[:print:]' | head -c 40)
      case "$code" in
        *redirect_uri_mismatch*) printf 'missing' ;;
        # A wrong client id is at least as likely as a wrong URL, and it needs a different
        # answer: reporting it as a refused URI sends people to check a URL that is fine.
        *invalid_client*)        printf 'unknown_client' ;;
        *)                       printf 'refused' ;;
      esac
      ;;
    *) printf 'registered' ;;
  esac
}

printf '{"linking":"%s","login":"%s","public_url":"%s"}\n' \
  "$(probe "$PUBLIC_URL/accounts/link/google/callback")" \
  "$(probe "$PUBLIC_URL/auth/google/callback")" \
  "$PUBLIC_URL"
