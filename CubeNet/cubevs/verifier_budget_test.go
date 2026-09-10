package cubevs

// Loads every production object so the verifier runs over it, and guards the
// instruction budget. `make verify` (veristat) reports the same numbers in more
// detail; this keeps a cheap regression check in the normal test run, since the
// DNS pipeline is split across tail calls precisely to stay inside the budget
// and a refactor that merges programs can blow it silently.

import (
	"testing"

	"github.com/cilium/ebpf"
)

// verifierInsnBudget leaves generous headroom under the kernel's 1M complexity
// limit: crossing it means the split between tail-called programs needs
// revisiting, not that the limit is close.
const verifierInsnBudget = 800_000

func TestVerifierInstructionBudget(t *testing.T) {
	objects := map[string]func() (*ebpf.CollectionSpec, error){
		"localgw": loadLocalgw,
		"mvmtap":  loadMvmtap,
		"nodenic": loadNodenic,
	}
	for name, load := range objects {
		t.Run(name, func(t *testing.T) {
			spec, err := load()
			if err != nil {
				t.Fatalf("load spec: %v", err)
			}
			// Load into anonymous maps: the test must never touch the pins a
			// running Cubelet owns.
			for _, m := range spec.Maps {
				m.Pinning = ebpf.PinNone
			}
			coll, err := ebpf.NewCollection(spec)
			if err != nil {
				if bpfTestUnavailable(err) {
					t.Skipf("kernel BPF unavailable: %v", err)
				}
				t.Fatalf("load collection: %v", err)
			}
			defer coll.Close()

			for progName, prog := range coll.Programs {
				info, err := prog.Info()
				if err != nil {
					continue
				}
				insns, ok := info.VerifiedInstructions()
				if !ok {
					continue
				}
				t.Logf("%-28s verified_insns=%d", progName, insns)
				if insns > verifierInsnBudget {
					t.Errorf("%s verified %d instructions, budget is %d",
						progName, insns, verifierInsnBudget)
				}
			}
		})
	}
}
