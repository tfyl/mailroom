#!/bin/sh
# Refreshes the vendored copy of Basecoat under internal/web/assets/basecoat.
#
# Basecoat is shadcn/ui's look expressed as plain CSS, which is the whole reason this UI can
# have it: shadcn itself is React, and this interface ships no JavaScript at all. Upstream is
# taken from npm rather than a CDN because what is served has to be in the repository — the
# policy is default-src 'none', so a stylesheet from anywhere but this origin would not load
# even if reaching for one were a good idea.
#
# Only the components this UI is allowed to use are vendored, and the list below is why:
# every other Basecoat component is inert without basecoat.js. Not vendoring them is a
# stronger statement than a note in the docs, because there is then nothing to import.
#
# Run it, look at the diff, then commit both the vendored CSS and the rebuilt
# internal/web/static/app.css that `make css` produces from it.
set -eu

VERSION="${BASECOAT_VERSION:-1.0.2}"
# Basecoat calls these "styles": one set of colours, sizes and radii over a shared structure.
# vega is its rendering of shadcn/ui's own default look.
STYLE="${BASECOAT_STYLE:-vega}"

# Every component whose behaviour is CSS alone. Adding one here is the first half of adding
# it to the UI; the second is an @import in internal/web/assets/app.css.
#
# Deliberately absent, and to stay absent: accordion, chart, combobox, command, dialog,
# drawer, dropdown-menu, popover, range, select, sidebar, tabs, toast, tooltip.
# form.css is left out on purpose: it is a barrel that pulls in select, switch and range
# alongside the useful halves, and every piece worth having is imported by name below.
COMPONENTS="alert badge button button-group card checkbox empty field input item kbd
label native-select radio table textarea"

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
dest="$root/internal/web/assets/basecoat"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "fetching basecoat-css@$VERSION"
curl -fsSL "https://registry.npmjs.org/basecoat-css/-/basecoat-css-$VERSION.tgz" \
  -o "$work/basecoat.tgz"
tar xzf "$work/basecoat.tgz" -C "$work"
src="$work/package/dist"

rm -rf "$dest"
mkdir -p "$dest/components" "$dest/styles/$STYLE"

for c in $COMPONENTS; do
  cp "$src/components/$c.css" "$dest/components/$c.css"
done

# The skin arrives as one file covering every component at once, so it is split along the
# section comments upstream already writes. A vendored component with no section of its own
# is normal — several are structure only — but a section that stopped matching would leave
# its component unstyled, which is loud rather than subtle.
awk -v dir="$dest/styles/$STYLE" -v style="$STYLE" '
  function slug(s,   t) {
    t = tolower(s); gsub(/[^a-z0-9]+/, "-", t); gsub(/^-|-$/, "", t); return t
  }
  { line[NR] = $0 }
  END {
    last = NR
    while (last > 0 && line[last] ~ /^[[:space:]]*$/) last--
    for (i = 1; i <= last; i++) {
      l = line[i]
      if (l == "@layer components {") continue
      if (i == last && l == "}") continue
      if (l ~ /^[[:space:]]*\/\*[[:space:]]*[A-Za-z][A-Za-z ]*\*\/[[:space:]]*$/) {
        name = l
        sub(/^[[:space:]]*\/\*[[:space:]]*/, "", name)
        sub(/[[:space:]]*\*\/[[:space:]]*$/, "", name)
        if (f != "") { print "}" > f; close(f) }
        f = dir "/" slug(name) ".css"
        print "/* Basecoat " style " — " name ". Vendored by scripts/vendor-basecoat.sh. */" > f
        print "@layer components {" > f
        continue
      }
      if (f != "") print l > f
    }
    if (f != "") { print "}" > f; close(f) }
  }
' "$src/styles/$STYLE.css"

# Whatever the split produced for a component that is not vendored is dead weight, and worse,
# it would style markup no structure file here can support.
for f in "$dest/styles/$STYLE"/*.css; do
  name=$(basename "$f" .css)
  keep=""
  for c in $COMPONENTS; do
    [ "$c" = "$name" ] && keep=yes
  done
  [ -n "$keep" ] || rm -f "$f"
done

cp "$work/package/LICENSE.md" "$dest/LICENSE.md"
printf '%s\n' "basecoat-css $VERSION, style: $STYLE" > "$dest/VERSION"

echo "vendored into $dest:"
(cd "$dest" && ls components "styles/$STYLE")
echo
echo "now run: make css"
