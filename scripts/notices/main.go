// Command notices writes the third-party licence notices that have to travel with a
// mailroom binary.
//
// Every dependency mailroom uses is MIT, BSD or Apache-2.0, and all three ask for the
// copyright notice and the permission text to accompany copies of the software — including
// copies in binary form. Go puts neither into a compiled binary, so a published image
// carries the code and none of the notices unless something puts them there. This produces
// the file that does.
//
// It lives in its own module, with its own go.mod and go.sum, so that the licence detector
// it uses is pinned and checksum-verified without becoming a dependency of the server. A
// nested module is invisible to `go build ./...` and `go test ./...` at the root, which is
// what keeps a clone buildable with nothing but Go.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.elastic.co/go-licence-detector/dependency"
	"go.elastic.co/go-licence-detector/detector"
)

const mainModule = "github.com/tfyl/mailroom"

// allowed is the set of licences a dependency of mailroom may be under. A module whose
// licence is not here stops the build rather than appearing in the output.
//
// Written out here rather than taken from the detector's own embedded list, so that the
// policy is in this repository and widening it is a diff somebody reviews. Everything on it
// is permissive and compatible in the direction that has to hold: mailroom may take
// Apache-2.0 or BSD code into an AGPL-3.0 binary, and a copyleft dependency would be a
// decision rather than an accident.
//
// The two compound entries are single LICENSE files that genuinely contain two licences. See
// identified below for which, and why they are named that way.
var allowed = detector.Rules{
	AllowList: map[string]struct{}{
		"Apache-2.0":                  {},
		"Apache-2.0 AND BSD-3-Clause": {},
		"Apache-2.0 AND MIT":          {},
		"BSD-2-Clause":                {},
		"BSD-3-Clause":                {},
		"ISC":                         {},
		"MIT":                         {},
	},
}

// identified records the licences a person read and named, where the classifier's answer was
// wrong or was only part of the answer.
//
// Only the identification is overridden. The licence text still comes from the module in the
// cache, so an upgrade that changes the terms changes what is reproduced, and an entry here
// that has gone stale can only mislabel a text that is still correct — it cannot suppress
// one. Nothing is listed for convenience; each is a file somebody opened.
var identified = dependency.Overrides{
	// Read as BSD-2-Clause, and they are not. All three carry three conditions including
	// non-endorsement; the classifier scores the third below its threshold because it is
	// worded "Neither the names of the authors nor the names of the contributors" rather
	// than the canonical "Neither the name of the copyright holder". libc's own
	// LICENSE-3RD-PARTY.md says in as many words that the main project is BSD-3.
	"modernc.org/libc":     {LicenceType: "BSD-3-Clause"},
	"modernc.org/mathutil": {LicenceType: "BSD-3-Clause"},
	"modernc.org/memory":   {LicenceType: "BSD-3-Clause"},

	// One LICENSE file holding two complete licences: the Apache-2.0 text, a rule, then a
	// full BSD-3-Clause block for the parts derived from the Go standard library. The
	// reproduced text has always carried both — this is so the summary does not claim the
	// file says only one thing, which is the part a downstream compliance scan reads. Their
	// sibling go.opentelemetry.io/auto/sdk is plain Apache-2.0 and is deliberately absent.
	"go.opentelemetry.io/otel":                                      {LicenceType: "Apache-2.0 AND BSD-3-Clause"},
	"go.opentelemetry.io/otel/metric":                               {LicenceType: "Apache-2.0 AND BSD-3-Clause"},
	"go.opentelemetry.io/otel/trace":                                {LicenceType: "Apache-2.0 AND BSD-3-Clause"},
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp": {LicenceType: "Apache-2.0 AND BSD-3-Clause"},

	// Three licences in one file: a preamble about a relicensing transition, Apache-2.0,
	// then a complete MIT text. The preamble says contributions from authors who did not
	// consent to the relicensing remain under MIT, so both cover the code. The CC-BY-4.0 the
	// same file mentions covers documentation, which is not in the binary.
	"github.com/modelcontextprotocol/go-sdk": {LicenceType: "Apache-2.0 AND MIT"},
}

// licenceFile matches the names licence texts are kept under. NOTICE is deliberately not in
// it: an Apache-2.0 NOTICE is a different requirement, and is handled separately.
var licenceFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|copyright)([-._].*)?$`)

// isLicence decides whether a file name is a licence text, which the pattern above is not
// quite enough to answer: the MCP SDK has a copyright_test.go that matches it and is a Go
// test. Nothing with a code extension is a licence, and the ones licences do use are few
// enough to name.
func isLicence(name string) bool {
	if !licenceFile.MatchString(name) {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case "", ".md", ".txt", ".rst":
		return true
	}
	return false
}

func main() {
	root := flag.String("root", "", "path to the repository root")
	pkg := flag.String("command", mainModule+"/cmd/mailroom",
		"the command whose linked dependencies are listed")
	out := flag.String("out", "", "path to write the notices to; - for stdout")
	flag.Parse()

	if *root == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "both -root and -out are required")
		os.Exit(2)
	}

	text, err := generate(*root, *pkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *out == "-" {
		os.Stdout.Write(text)
		return
	}
	if err := os.WriteFile(*out, text, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(root, pkg string) ([]byte, error) {
	linked, err := linkedPackages(root, pkg)
	if err != nil {
		return nil, err
	}
	if len(linked) == 0 {
		return nil, fmt.Errorf("%s links no third-party modules, which cannot be right", pkg)
	}

	modules := make([]string, 0, len(linked))
	for module := range linked {
		modules = append(modules, module)
	}
	sort.Strings(modules)

	described, err := goCommand(root, append([]string{"list", "-m", "-json"}, modules...)...)
	if err != nil {
		return nil, err
	}

	classifier, err := detector.NewClassifier("")
	if err != nil {
		return nil, fmt.Errorf("licence classifier: %w", err)
	}

	// includeIndirect, because "indirect" is a statement about go.mod and not about the
	// binary. A module reached only through another dependency is linked in exactly as much
	// as one imported by name here, and its licence asks for exactly as much.
	list, err := detector.Detect(bytes.NewReader(described), classifier, &allowed, identified, true)
	if err != nil {
		return nil, err
	}

	deps := append(append([]dependency.Info{}, list.Direct...), list.Indirect...)
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	if len(deps) != len(modules) {
		return nil, fmt.Errorf("%d modules link in but only %d were described; a dependency has gone missing",
			len(modules), len(deps))
	}

	return render(deps, linked)
}

// linkedPackages returns, for every third-party module the given command actually links, the
// directories of the packages it links from it. The main module is excluded: it is the thing
// being licensed rather than a third party to it.
//
// `go list -deps` on the command rather than a read of go.mod, because those are different
// sets and only one of them is redistributed. go.mod names everything the module graph needs
// to resolve, which includes dependencies of the test files of dependencies, and tooling that
// only ever runs at build time. None of that reaches the binary, and listing it would claim
// obligations mailroom does not have while saying nothing about the ones it does.
//
// The package directories are kept, and not only the module paths, because a module can hold
// code under a licence of its own in a subdirectory — a fork of a standard library package,
// or a third party vendored into it — and whether that code ships depends on whether its
// package is one of the ones linked.
func linkedPackages(root, pkg string) (map[string][]string, error) {
	const format = "{{if and (not .Standard) .Module}}{{.Module.Path}}\t{{.Dir}}{{end}}"
	stdout, err := goCommand(root, "list", "-deps", "-f", format, pkg)
	if err != nil {
		return nil, err
	}

	linked := map[string][]string{}
	for _, line := range strings.Split(string(stdout), "\n") {
		module, dir, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || module == mainModule {
			continue
		}
		linked[module] = append(linked[module], dir)
	}
	return linked, nil
}

func goCommand(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout, nil
}

func render(deps []dependency.Info, linked map[string][]string) ([]byte, error) {
	var b bytes.Buffer

	b.WriteString(`# Third-party notices

mailroom is a single statically linked binary, and the Go modules below are compiled into
it. Every one of them is under a licence that asks for its copyright notice and its
permission text to accompany copies of the software, and a container image is a copy. Go
puts nothing of the sort into a binary by itself, so this file is what carries them.

The list is exactly what ` + "`go list -deps`" + ` reports for ` + "`./cmd/mailroom`" + `, which is narrower than
go.mod: a module needed only to build or to test something is never redistributed and is not
listed here. Licence texts are reproduced from the module sources in the Go module cache
rather than fetched from anywhere, so they are the texts of the versions that were built.

Where a module carries a second licence beside its own — a fork of a standard library
package, or a third party vendored into it — that text is reproduced too, but only when the
package holding it is one the binary actually links.

Generated by ` + "`make notices`" + `. Do not edit it by hand — ` + "`make notices-check`" + ` runs in CI and
fails when this file is not what the dependency graph produces.

`)

	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "%d modules, by licence:\n\n", len(deps))
	b.WriteString("| Licence | Modules |\n|---|---:|\n")
	for _, count := range countByLicence(deps) {
		fmt.Fprintf(&b, "| %s | %d |\n", count.licence, count.n)
	}

	b.WriteString("\n## Modules\n")
	for _, dep := range deps {
		licence, err := os.ReadFile(dep.LicenceFile)
		if err != nil {
			return nil, fmt.Errorf("reading the licence of %s: %w", dep.Name, err)
		}

		fmt.Fprintf(&b, "\n### %s\n\n", dep.Name)
		fmt.Fprintf(&b, "%s &middot; %s &middot; <%s>\n\n", dep.Version, dep.LicenceType, dep.URL)
		fmt.Fprintf(&b, "Licence text, from `%s`:\n\n", relativeTo(dep.Dir, dep.LicenceFile))
		writeVerbatim(&b, licence)

		// Apache-2.0 section 4(d) is a separate requirement from the licence text: a module
		// that ships a NOTICE file has the attribution notices in it carried into anything
		// that redistributes the module. Reproducing the licence alone would not satisfy it,
		// and the file is easy to miss because most modules do not have one.
		notice, name, err := findNotice(dep.Dir)
		if err != nil {
			return nil, fmt.Errorf("reading the NOTICE of %s: %w", dep.Name, err)
		}
		if notice != nil {
			fmt.Fprintf(&b, "Attribution notices, from `%s`, required by Apache-2.0 section 4(d):\n\n", name)
			writeVerbatim(&b, notice)
		}

		others, err := otherLicences(dep, linked[dep.Name])
		if err != nil {
			return nil, fmt.Errorf("reading the other licences of %s: %w", dep.Name, err)
		}
		for _, other := range others {
			fmt.Fprintf(&b, "Also carried by this module, from `%s`:\n\n", other.name)
			writeVerbatim(&b, other.text)
		}
	}

	return b.Bytes(), nil
}

type licenceCount struct {
	licence string
	n       int
}

func countByLicence(deps []dependency.Info) []licenceCount {
	n := map[string]int{}
	for _, dep := range deps {
		n[dep.LicenceType]++
	}
	counts := make([]licenceCount, 0, len(n))
	for licence, count := range n {
		counts = append(counts, licenceCount{licence, count})
	}
	// By count and then by name, so that the table is stable: ranging a map is not, and an
	// unstable order would make every regeneration look like a change.
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].n != counts[j].n {
			return counts[i].n > counts[j].n
		}
		return counts[i].licence < counts[j].licence
	})
	return counts
}

type otherLicence struct {
	name string
	text []byte
}

// otherLicences finds the licence texts a module carries besides the one that describes the
// module as a whole.
//
// Two shapes, and both cover code that ships. A module root can hold more than one file:
// musl's MIT terms sit beside modernc.org/libc's own BSD, and libc is a translation of musl,
// so those terms cover most of what is in the binary. And a subdirectory can hold its own,
// which is how a fork of a standard library package arrives — go-jose keeps one under json/,
// google.golang.org/api keeps one under internal/third_party/uritemplates, and both name
// copyright holders who appear nowhere else in this file.
//
// A subdirectory is only searched when a package linked from it is in the dependency list,
// which is what keeps the answer about the binary rather than about the module zip.
// modernc.org/mathutil's mersenne/LICENSE and segmentio/encoding's json/fuzz/LICENSE are
// both real licences over code nothing here compiles, and neither appears.
func otherLicences(dep dependency.Info, packages []string) ([]otherLicence, error) {
	directories := map[string]bool{dep.Dir: true}
	for _, pkg := range packages {
		// Every directory from the package up to the module root, because a licence covering
		// a package can sit at the top of the subtree it belongs to rather than beside it.
		for dir := pkg; dir != dep.Dir && strings.HasPrefix(dir, dep.Dir); dir = filepath.Dir(dir) {
			directories[dir] = true
		}
	}

	var found []otherLicence
	for dir := range directories {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() || path == dep.LicenceFile || !isLicence(entry.Name()) {
				continue
			}
			text, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			found = append(found, otherLicence{relativeTo(dep.Dir, path), text})
		}
	}

	// Ranging a map is unordered, and an unordered list would make every regeneration look
	// like a change.
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	return found, nil
}

// findNotice looks for an Apache-2.0 NOTICE beside the module's licence. Only the module
// root is searched, because that is where section 4(d) says it lives — a file called NOTICE
// deeper in a tree is as likely to be a note to contributors as an attribution notice.
func findNotice(dir string) ([]byte, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch strings.ToUpper(name) {
		case "NOTICE", "NOTICE.TXT", "NOTICE.MD":
			text, err := os.ReadFile(filepath.Join(dir, name))
			return text, name, err
		}
	}
	return nil, "", nil
}

func relativeTo(dir, file string) string {
	if rel, err := filepath.Rel(dir, file); err == nil {
		return rel
	}
	return filepath.Base(file)
}

// writeVerbatim puts a licence text in a fenced block, so that it is reproduced exactly
// rather than rendered — a licence with an underscore or an asterisk in it is still the
// licence, and markdown would otherwise eat both.
//
// The fence is made longer than the longest run of backticks in the text. A licence
// containing a fence of its own is unlikely, but a notice that silently swallowed the rest
// of the file would be the kind of failure nobody looks for.
func writeVerbatim(b *bytes.Buffer, text []byte) {
	fence := strings.Repeat("`", longestBacktickRun(text)+1)
	b.WriteString(fence)
	b.WriteString("text\n")
	b.Write(bytes.TrimRight(text, "\n"))
	b.WriteString("\n")
	b.WriteString(fence)
	b.WriteString("\n\n")
}

func longestBacktickRun(text []byte) int {
	longest, run := 2, 0
	for _, c := range text {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	return longest
}
