package cubevs

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	typeNameU32           = "__u32"
	typeNameLPMKey        = "lpm_key"
	typeNamePolicyValue   = "net_policy_value_v2"
	typeNameDNSAllowKey   = "dns_allow_key"
	typeNameDNSAllowValue = "dns_allow_value"
)

var (
	btfTypeU32           btf.Type
	btfTypeLPMKey        btf.Type
	btfTypePolicyValue   btf.Type
	btfTypeDNSAllowKey   btf.Type
	btfTypeDNSAllowValue btf.Type
)

func init() {
	_ = rlimit.RemoveMemlock()
}

func rewriteConstants(vars map[string]*ebpf.VariableSpec, params Params) error {
	var err error
	err = errors.Join(err, vars[globalNameMVMInnerIP].Set(ipToUint32(params.MVMInnerIP)))
	err = errors.Join(err, vars[globalNameMVMMacaddrP1].Set(hardwareAddrToUint32(params.MVMMacAddr)))
	err = errors.Join(err, vars[globalNameMVMMacaddrP2].Set(hardwareAddrToUint16(params.MVMMacAddr)))
	err = errors.Join(err, vars[globalNameMVMGatewayIP].Set(ipToUint32(params.MVMGatewayIP)))
	err = errors.Join(err, vars[globalNameCubegw0IP].Set(ipToUint32(params.Cubegw0IP)))
	err = errors.Join(err, vars[globalNameCubegw0Ifindex].Set(params.Cubegw0Ifindex))
	err = errors.Join(err, vars[globalNameCubegw0MacaddrP1].Set(hardwareAddrToUint32(params.Cubegw0MacAddr)))
	err = errors.Join(err, vars[globalNameCubegw0MacaddrP2].Set(hardwareAddrToUint16(params.Cubegw0MacAddr)))
	err = errors.Join(err, vars[globalNameNodeIP].Set(ipToUint32(params.NodeIP)))
	err = errors.Join(err, vars[globalNameNodeIfindex].Set(params.NodeIfindex))
	err = errors.Join(err, vars[globalNameNodeMacaddrP1].Set(hardwareAddrToUint32(params.NodeMacAddr)))
	err = errors.Join(err, vars[globalNameNodeMacaddrP2].Set(hardwareAddrToUint16(params.NodeMacAddr)))
	err = errors.Join(err, vars[globalNameNodeGatewayMacaddrP1].Set(hardwareAddrToUint32(params.NodeGatewayMacAddr)))
	err = errors.Join(err, vars[globalNameNodeGatewayMacaddrP2].Set(hardwareAddrToUint16(params.NodeGatewayMacAddr)))
	return err
}

func pinProgs(obj *ebpf.Collection) error {
	for progName, prog := range obj.Programs {
		pinnedPath := pinPath(progName)
		_ = os.Remove(pinnedPath) // NOCC:Path Traversal()
		err := prog.Pin(pinnedPath)
		if err != nil {
			return fmt.Errorf("ebpf.Program.Pin failed: %w, name: %s", err, progName)
		}
	}
	return nil
}

type dnsTailCallBinding struct {
	slot        uint32
	programName string
}

func dnsTailCallBindings() []dnsTailCallBinding {
	return []dnsTailCallBinding{
		{dnsTailCallParse, programNameDNSParseChunk},
		{dnsTailCallReverse, programNameDNSRevChunk},
		{dnsTailCallFinish, programNameDNSFinish},
		{dnsTailCallResponse, programNameDNSHandleResponse},
		{dnsTailCallResponseFinish, programNameDNSResponseFinish},
	}
}

// populateDNSTailCalls binds DNS tail-call slots to their BPF programs.
//
// map.h is shared by multiple BPF objects, so the dns_tail_calls jump table
// shows up in every spec. Each object only owns a subset of the tail-called
// programs (mvmtap owns the query pipeline, nodenic owns the response
// handler), so we register only the bindings the current spec can satisfy.
// The remaining slots get populated at runtime via refreshDNSTailCalls once
// the other objects have been loaded and pinned.
func populateDNSTailCalls(spec *ebpf.CollectionSpec) error {
	jumpTable, ok := spec.Maps[mapNameDNSTailCalls]
	if !ok {
		return nil
	}

	// Rebuild static contents so the object loads with a deterministic jump table.
	jumpTable.Contents = jumpTable.Contents[:0]
	for _, binding := range dnsTailCallBindings() {
		if _, ok := spec.Programs[binding.programName]; !ok {
			continue
		}
		jumpTable.Contents = append(jumpTable.Contents, ebpf.MapKV{
			Key:   binding.slot,
			Value: binding.programName,
		})
	}
	return nil
}

func isPinnedObjectNotExist(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) || strings.Contains(err.Error(), "no such file or directory"))
}

func refreshDNSTailCalls() error {
	jumpTable, err := loadPinnedMap(mapNameDNSTailCalls)
	if err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) || isPinnedObjectNotExist(err) {
			return nil
		}
		return err
	}
	defer jumpTable.Close()

	for _, binding := range dnsTailCallBindings() {
		prog, err := ebpf.LoadPinnedProgram(pinPath(binding.programName), nil)
		if err != nil {
			// Programs are pinned by different objects (mvmtap owns the
			// query pipeline, nodenic owns the response handler), so a
			// missing pin just means the owning object hasn't been
			// loaded yet. A later refresh will fill the slot in.
			if isPinnedObjectNotExist(err) {
				continue
			}
			return fmt.Errorf("ebpf.LoadPinnedProgram failed: %w, name: %s", err, binding.programName)
		}

		if err := jumpTable.Update(&binding.slot, prog, ebpf.UpdateAny); err != nil {
			prog.Close()
			return fmt.Errorf("map.Update failed: %w, name: %s, slot: %d, program: %s", err, mapNameDNSTailCalls, binding.slot, binding.programName)
		}
		prog.Close()
	}
	return nil
}

func loadObject(params Params, loader func() (*ebpf.CollectionSpec, error), name string) error {
	opts := ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: bpfFSPath,
		},
	}

	spec, err := loader()
	if err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}

	btfSpec := spec.Types
	iter := btfSpec.Iterate()
	for iter.Next() {
		if iter.Type.TypeName() == typeNameU32 {
			btfTypeU32 = iter.Type
		}
		if iter.Type.TypeName() == typeNameLPMKey {
			btfTypeLPMKey = iter.Type
		}
		if iter.Type.TypeName() == typeNamePolicyValue {
			btfTypePolicyValue = iter.Type
		}
		if iter.Type.TypeName() == typeNameDNSAllowKey {
			btfTypeDNSAllowKey = iter.Type
		}
		if iter.Type.TypeName() == typeNameDNSAllowValue {
			btfTypeDNSAllowValue = iter.Type
		}
	}

	err = populateDNSTailCalls(spec)
	if err != nil {
		return fmt.Errorf("%s populateDNSTailCalls failed: %w", name, err)
	}

	err = rewriteConstants(spec.Variables, params)
	if err != nil {
		return fmt.Errorf("%s rewriteConstants failed: %w", name, err)
	}

	obj, err := ebpf.NewCollectionWithOptions(spec, opts)
	if err != nil {
		return fmt.Errorf("ebpf.NewCollectionWithOptions: %w", err)
	}
	defer obj.Close()

	return pinProgs(obj)
}

func attachTCFilter(progName string, ifindex uint32, direction TCDirection) error {
	prog, err := ebpf.LoadPinnedProgram(pinPath(progName), nil)
	if err != nil {
		return fmt.Errorf("ebpf.LoadPinnedProgram failed: %w, name: %s", err, progName)
	}
	defer prog.Close()

	err = createQdisc(ifindex)
	if err != nil {
		return err
	}

	err = attachFilter(ifindex, uint32(prog.FD()), progName, direction)
	if err != nil {
		return err
	}
	return nil
}

// Init should be called once before invoking any other CubeVS APIs.
func Init(params Params) error {
	_ = os.Remove(pinPath("tungrp_to_tuns")) // NOCC:Path Traversal()
	// dns_query_track is runtime pending-query state, not persisted policy.
	_ = os.Remove(pinPath(MapNameDNSQueryTrack)) // NOCC:Path Traversal()

	err := loadObject(params, loadLocalgw, "loadLocalgw")
	if err != nil {
		return err
	}

	err = loadObject(params, loadMvmtap, "loadMvmtap")
	if err != nil {
		return err
	}
	if err := refreshDNSTailCalls(); err != nil {
		return err
	}

	err = loadObject(params, loadNodenic, "loadNodenic")
	if err != nil {
		return err
	}
	// Re-run refresh now that nodenic's response handler is pinned, so the
	// DNS_TAIL_CALL_RESPONSE slot owned by nodenic gets wired up at runtime.
	if err := refreshDNSTailCalls(); err != nil {
		return err
	}

	if err := migrateAllowOutV1ToV2(); err != nil {
		return err
	}

	// attach TC filter to cube-dev
	err = attachTCFilter(programNameFromEnvoy, params.Cubegw0Ifindex, TCEgress)
	if err != nil {
		return err
	}

	// attach TC filter to eth0
	err = attachTCFilter(programNameFromWorld, params.NodeIfindex, TCIngress)
	if err != nil {
		return err
	}

	return nil
}

// AttachFilter attaches a BPF TC filter to the ingress path of the TAP device specified by ifindex.
func AttachFilter(ifindex uint32) error {
	prog, err := ebpf.LoadPinnedProgram(pinPath(programNameFromCube), nil)
	if err != nil {
		return fmt.Errorf("ebpf.LoadPinnedProgram failed: %w, name: %s", err, programNameFromCube)
	}
	defer prog.Close()

	err = createQdisc(ifindex)
	if err != nil {
		return err
	}

	err = attachFilter(ifindex, uint32(prog.FD()), programNameFromCube, TCIngress)
	if err != nil {
		return err
	}

	return initNetPolicy(ifindex)
}
