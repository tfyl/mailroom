package notices

import (
	"os/exec"
	"strings"
	"testing"
)

// The linked set is the whole point: a module that ends up in the binary is redistributed
// and needs its notice, and a module that does not is not and should not be listed. So this
// asks the go command what ./cmd/mailroom actually links and requires the notice file to
// account for all of it.
//
// `make notices-check` is the stronger check — it regenerates the file and compares byte for
// byte, so it also catches a version bump or a re-licensed dependency. It needs the
// generator's own module and a network fetch on a cold cache. This one runs on a clone with
// nothing but Go, which is the only toolchain the project asks a contributor for, and it is
// the one that catches the mistake somebody is actually going to make: adding a dependency
// and not regenerating.
func TestEveryLinkedModuleHasItsNotice(t *testing.T) {
	const format = `{{if and (not .Standard) .Module}}{{.Module.Path}}{{end}}`
	out, err := exec.Command("go", "list", "-deps", "-f", format,
		"github.com/tfyl/mailroom/cmd/mailroom").Output()
	if err != nil {
		t.Fatalf("asking the go command what the binary links: %v", err)
	}

	seen := map[string]bool{}
	var linked int
	for _, line := range strings.Split(string(out), "\n") {
		module := strings.TrimSpace(line)
		// The main module is the thing being licensed, not a third party to it.
		if module == "" || module == "github.com/tfyl/mailroom" || seen[module] {
			continue
		}
		seen[module] = true
		linked++
		if !strings.Contains(Text, "\n### "+module+"\n") {
			t.Errorf("%s links into the binary and NOTICES.md does not carry its licence. Run: make notices", module)
		}
	}

	if linked == 0 {
		t.Fatal("the binary appears to link no third-party modules, so this test proved nothing")
	}
}

// The other direction. A dependency dropped from the binary leaves a licence text behind
// that describes software nobody is being given, which is a smaller problem than a missing
// notice but the same staleness — and it is the half a regeneration would fix silently, so
// without this the file could name a module that has not been shipped for a year.
func TestTheNoticeListsNothingThatIsNotLinked(t *testing.T) {
	const format = `{{if and (not .Standard) .Module}}{{.Module.Path}}{{end}}`
	out, err := exec.Command("go", "list", "-deps", "-f", format,
		"github.com/tfyl/mailroom/cmd/mailroom").Output()
	if err != nil {
		t.Fatalf("asking the go command what the binary links: %v", err)
	}

	linked := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if module := strings.TrimSpace(line); module != "" {
			linked[module] = true
		}
	}

	for _, line := range strings.Split(Text, "\n") {
		module, found := strings.CutPrefix(line, "### ")
		if !found {
			continue
		}
		if !linked[module] {
			t.Errorf("NOTICES.md carries a licence for %s, which the binary no longer links. Run: make notices", module)
		}
	}
}
