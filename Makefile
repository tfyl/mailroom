# Everything here is optional. `go build ./...` and `go test ./...` work on a fresh clone
# with no tools beyond Go, because the two generated files in the repository —
# internal/web/static/app.css and internal/notices/NOTICES.md — are committed. These targets
# are for changing them.

TAILWIND_VERSION := 4.3.3

# Downloaded, never installed: it lands in a gitignored directory inside the checkout and
# nothing outside the repository is touched.
TOOLS := .tools
TAILWIND := $(TOOLS)/tailwindcss-$(TAILWIND_VERSION)
TAILWIND_SUMS := tailwind.sha256

CSS_IN  := internal/web/assets/app.css
CSS_OUT := internal/web/static/app.css
CSS_DEPS := $(shell find internal/web/assets -name '*.css') $(wildcard internal/web/templates/*.html)

NOTICES_OUT := internal/notices/NOTICES.md
NOTICES_TOOL := scripts/notices

.PHONY: help css css-check vendor-css notices notices-check readme-shots ui-shots build test

help:
	@echo 'css            rebuild $(CSS_OUT) from internal/web/assets'
	@echo 'css-check      fail if the committed stylesheet is not what the sources produce'
	@echo 'vendor-css     refresh the vendored copy of Basecoat, then rebuild'
	@echo 'notices        rebuild $(NOTICES_OUT) from the dependency graph'
	@echo 'notices-check  fail if the committed notices are not what the graph produces'
	@echo 'readme-shots   rebuild the pictures README.md shows'
	@echo 'ui-shots       rebuild the UI review set under docs/ui/screenshots'
	@echo 'build          go build'
	@echo 'test           go test'

css: $(CSS_OUT)

# Basecoat is MIT, and MIT asks for its copyright and permission notice to accompany every
# copy of the software. $(CSS_OUT) is a copy: it is the file the binary embeds and the only
# thing a browser ever receives, and minification leaves nothing of the vendored sources'
# comments in it. So the notice is put back afterwards, alongside the banner Tailwind writes
# for itself.
#
# Both halves are read out of the vendored tree rather than written here, so a `make
# vendor-css` that lands on a new release moves the banner with it. A version restated in
# this file would be a second place to remember and the first one to go stale.
BASECOAT := internal/web/assets/basecoat
BASECOAT_VERSION = $(shell sed -n 's/^basecoat-css \([^,]*\).*/\1/p' $(BASECOAT)/VERSION)
BASECOAT_COPYRIGHT = $(shell sed -n '/^Copyright (c)/{p;q;}' $(BASECOAT)/LICENSE.md)
BASECOAT_NOTICE = /*! Basecoat $(BASECOAT_VERSION) | MIT | $(BASECOAT_COPYRIGHT) */

# `/*!` rather than `/*` is the marker CSS and JS minifiers honour for a licence comment, and
# it is the shape Tailwind's own banner takes. Nothing here depends on that — the notice is
# prepended to output the minifier has already finished with — but keeping the marker means
# any later tool that does run over this file leaves it alone too.
#
# One definition, used by the build and by the check, so the check cannot pass a stylesheet
# the build would not produce. $1 is the file to rewrite in place.
notice = { printf '%s\n' '$(BASECOAT_NOTICE)'; cat $(1); } > $(1).notice && mv $(1).notice $(1)

# The templates are a dependency because Tailwind only emits the utilities it finds used, so
# a class added to a page changes the output as surely as a change to the sources does. The
# two vendored files the banner is derived from are dependencies for the same reason: a
# Basecoat upgrade changes the output even when no rule in it moved.
$(CSS_OUT): $(TAILWIND) $(CSS_DEPS) $(BASECOAT)/VERSION $(BASECOAT)/LICENSE.md
	@mkdir -p $(dir $@)
	$(TAILWIND) --input $(CSS_IN) --output $@ --minify
	@$(call notice,$@)

# What CI runs. The stylesheet is committed, so it can drift from the sources it was built
# from — and drift would only show up as a page that quietly stopped matching its markup.
#
# The notice is checked before the comparison only so the failure names itself: losing it
# would fail the comparison anyway, with a message about drift that says nothing about a
# licence. internal/web carries the same assertion as a Go test, which is the one that still
# runs on a machine with no Tailwind binary.
css-check: $(TAILWIND) $(BASECOAT)/VERSION $(BASECOAT)/LICENSE.md
	@$(TAILWIND) --input $(CSS_IN) --output $(TOOLS)/app.check.css --minify --silent
	@$(call notice,$(TOOLS)/app.check.css)
	@if ! grep -qF -- '$(BASECOAT_NOTICE)' $(CSS_OUT); then \
		echo "$(CSS_OUT) does not carry the Basecoat licence notice MIT requires. Run: make css"; \
		exit 1; \
	fi
	@if ! cmp -s $(TOOLS)/app.check.css $(CSS_OUT); then \
		echo "$(CSS_OUT) is not what internal/web/assets builds. Run: make css"; \
		exit 1; \
	fi
	@echo "$(CSS_OUT) is up to date"

vendor-css:
	sh scripts/vendor-basecoat.sh
	$(MAKE) css

# The licences of the Go modules that end up inside the binary. MIT, BSD and Apache-2.0 all
# ask for the copyright notice and permission text to accompany copies of the software, and a
# published image is a copy that Go puts none of them into. $(NOTICES_OUT) is what carries
# them; internal/notices embeds it so `mailroom notices` can print it.
#
# $(NOTICES_TOOL) is a module of its own so that the licence detector it uses is pinned and
# checksummed in a go.mod and go.sum of its own, without appearing in the root go.mod or in
# anything a contributor has to download to build the server. The dependency graph is not a
# file, so this cannot be an ordinary target with prerequisites: there is nothing to compare
# timestamps against, and it is cheap enough to run unconditionally.
notices:
	cd $(NOTICES_TOOL) && go run . -root $(CURDIR) -out $(CURDIR)/$(NOTICES_OUT)
	@echo "wrote $(NOTICES_OUT)"

# What CI runs, and the same shape as css-check above and for the same reason: a committed
# artefact drifts from what produced it, and this one drifts silently — a dependency added or
# bumped changes what is being redistributed while the notice file goes on describing the old
# set. A hand-maintained list would have this problem permanently, which is why the generator
# is the only way the file is written.
#
# internal/notices carries a weaker version of the same assertion as a Go test, which runs on
# a clone with nothing but Go: it checks that every linked module is named, without needing
# the detector to rebuild the texts.
notices-check:
	@mkdir -p $(TOOLS)
	@cd $(NOTICES_TOOL) && go run . -root $(CURDIR) -out $(CURDIR)/$(TOOLS)/NOTICES.check.md
	@if ! cmp -s $(TOOLS)/NOTICES.check.md $(NOTICES_OUT); then \
		echo "$(NOTICES_OUT) is not what the dependency graph produces. Run: make notices"; \
		diff -u $(NOTICES_OUT) $(TOOLS)/NOTICES.check.md | head -40; \
		exit 1; \
	fi
	@echo "$(NOTICES_OUT) is up to date"

# The pictures in README.md. Wants Node and a Chrome and nothing else — neither is needed to
# build or run mailroom, which is why this is a target rather than a step in anything.
readme-shots:
	node scripts/readme-shots.mjs

# The UI review set: every page in every state, light and dark, wide and narrow, and the two
# contact sheets. Same requirements as readme-shots and for the same reason. It runs the
# `shots` build tag itself, so this one command is the whole of regenerating docs/ui/screenshots
# — and it writes only into pages/, narrow/ and the contact sheets. before/ and fixes/ are
# history and are never rewritten; scripts/ui-shots.mjs says why at the point it would be.
#
# Takes state names to redo a subset: `node scripts/ui-shots.mjs consent held`.
ui-shots:
	node scripts/ui-shots.mjs

# Tailwind publishes a standalone binary with its own runtime inside it, which is the whole
# reason this repository has no package.json, no node_modules and no Node in the release
# image. There is nothing to install and nothing to keep in step with a lockfile.
#
# What the pinned version does not pin is the bytes. A release asset can be replaced and a tag
# can be moved, and this download is executed by `make css-check` on every push and every pull
# request, and on the machine of anybody who clones the repository and touches the stylesheet.
# So it is checked against $(TAILWIND_SUMS) first. It stays under a temporary name with no
# execute bit until it passes, which is what makes the check a gate rather than a report: on a
# mismatch there is nothing left behind for a later target to pick up and run.
#
# $(TAILWIND_SUMS) is a prerequisite so that editing a recorded hash re-fetches and re-checks
# the binary. Otherwise the one already sitting in $(TOOLS) would satisfy the target and the
# new value would never be tested against anything.
#
# sha256sum is coreutils and macOS does not ship it; shasum is Perl's and is what macOS has.
# Whichever is present is used, and a machine with neither is a refusal, because the remaining
# option would be to run the binary unverified.
$(TAILWIND): $(TAILWIND_SUMS)
	@mkdir -p $(TOOLS)
	@set -eu; \
	os=$$(uname -s); arch=$$(uname -m); \
	case "$$os" in \
	  Linux) o=linux;; \
	  Darwin) o=macos;; \
	  *) echo "no standalone Tailwind build for $$os"; exit 1;; \
	esac; \
	case "$$arch" in \
	  x86_64|amd64) a=x64;; \
	  arm64|aarch64) a=arm64;; \
	  *) echo "no standalone Tailwind build for $$arch"; exit 1;; \
	esac; \
	libc=""; \
	if [ "$$o" = linux ] && ! ldd /bin/sh 2>/dev/null | grep -q 'libc\.so\.6'; then libc="-musl"; fi; \
	artifact="tailwindcss-$$o-$$a$$libc"; \
	want=$$(awk -v v='$(TAILWIND_VERSION)' -v f="$$artifact" '$$1 == v && $$2 == f { print $$3 }' $(TAILWIND_SUMS)); \
	if [ -z "$$want" ]; then \
	  echo "$(TAILWIND_SUMS) records no checksum for $$artifact at Tailwind $(TAILWIND_VERSION)."; \
	  echo "If TAILWIND_VERSION was just bumped, rewrite that file from the new release's"; \
	  echo "sha256sums.txt; the header there says where to get it."; \
	  exit 1; \
	fi; \
	url="https://github.com/tailwindlabs/tailwindcss/releases/download/v$(TAILWIND_VERSION)/$$artifact"; \
	echo "downloading $$url"; \
	curl -fsSL "$$url" -o $@.tmp; \
	if command -v sha256sum >/dev/null 2>&1; then \
	  got=$$(sha256sum $@.tmp | cut -d' ' -f1); \
	elif command -v shasum >/dev/null 2>&1; then \
	  got=$$(shasum -a 256 $@.tmp | cut -d' ' -f1); \
	else \
	  rm -f $@.tmp; \
	  echo "verifying $$artifact needs sha256sum or shasum and this machine has neither"; \
	  exit 1; \
	fi; \
	if [ "$$got" != "$$want" ]; then \
	  rm -f $@.tmp; \
	  echo "$$artifact is not the binary $(TAILWIND_SUMS) records for Tailwind $(TAILWIND_VERSION)."; \
	  echo "  expected $$want"; \
	  echo "  got      $$got"; \
	  echo "Nothing has been made executable. Treat the download as untrusted until you know why."; \
	  exit 1; \
	fi; \
	chmod +x $@.tmp; \
	mv $@.tmp $@

build:
	CGO_ENABLED=0 go build ./...

test:
	go test ./...
