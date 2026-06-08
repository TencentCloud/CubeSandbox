package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"unsafe"

	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"golang.org/x/sys/unix"
)

const (
	// Not exposed by x/sys/unix — these are ethtool wire-protocol
	// constants from include/uapi/linux/ethtool.h, not syscall numbers.
	ETH_SS_FEATURES = 4
	ETH_GSTRING_LEN = 32

	featNameTxTCPMangleIDSegmentation = "tx-tcp-mangleid-segmentation"
)

// struct ifreq with an embedded pointer payload.
type ifreq struct {
	Name [unix.IFNAMSIZ]byte
	Data uintptr
	_    [16]byte
}

// struct ethtool_sset_info { __u32 cmd; __u32 reserved; __u64 sset_mask; __u32 data[]; }
type ethtoolSsetInfo struct {
	Cmd      uint32
	Reserved uint32
	SsetMask uint64
	Data     [1]uint32 // single set requested -> one entry
}

// struct ethtool_gstrings { __u32 cmd; __u32 string_set; __u32 len; __u8 data[]; }
type ethtoolGstringsHdr struct {
	Cmd       uint32
	StringSet uint32
	Len       uint32
}

// struct ethtool_set_features_block { __u32 valid, requested; }
type setFeatureBlock struct {
	Valid     uint32
	Requested uint32
}

func ifreqName(iface string) [unix.IFNAMSIZ]byte {
	var n [unix.IFNAMSIZ]byte
	copy(n[:unix.IFNAMSIZ-1], iface)
	return n
}

func ioctl(fd int, iface string, data unsafe.Pointer) error {
	req := ifreq{Name: ifreqName(iface), Data: uintptr(data)}
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCETHTOOL),
		uintptr(unsafe.Pointer(&req)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// featureCount returns how many netdev features the kernel knows about.
func featureCount(fd int, iface string) (uint32, error) {
	info := ethtoolSsetInfo{
		Cmd:      unix.ETHTOOL_GSSET_INFO,
		SsetMask: 1 << ETH_SS_FEATURES,
	}
	if err := ioctl(fd, iface, unsafe.Pointer(&info)); err != nil {
		return 0, fmt.Errorf("ETHTOOL_GSSET_INFO: %w", err)
	}
	if info.SsetMask == 0 {
		return 0, fmt.Errorf("kernel does not expose ETH_SS_FEATURES")
	}
	return info.Data[0], nil
}

// featureIndexCache memoizes featureIndex lookups keyed by feature name.
// The kernel's feature string table is stable for the lifetime of the process,
// so caching avoids repeated ETHTOOL_GSSET_INFO + ETHTOOL_GSTRINGS ioctls.
type featureIndexCacheEntry struct {
	idx   uint32
	total uint32
}

var (
	featureIndexCacheMu sync.RWMutex
	featureIndexCache   = map[string]featureIndexCacheEntry{}
)

// featureIndex finds the bit index of `name` in the kernel's feature string table.
func featureIndex(fd int, iface, name string) (uint32, uint32, error) {
	featureIndexCacheMu.RLock()
	if e, ok := featureIndexCache[name]; ok {
		featureIndexCacheMu.RUnlock()
		return e.idx, e.total, nil
	}
	featureIndexCacheMu.RUnlock()

	n, err := featureCount(fd, iface)
	if err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, fmt.Errorf("no features reported by kernel")
	}

	bufSize := int(unsafe.Sizeof(ethtoolGstringsHdr{})) + int(n)*ETH_GSTRING_LEN
	buf := make([]byte, bufSize)
	hdr := (*ethtoolGstringsHdr)(unsafe.Pointer(&buf[0]))
	hdr.Cmd = unix.ETHTOOL_GSTRINGS
	hdr.StringSet = ETH_SS_FEATURES
	hdr.Len = n

	if err := ioctl(fd, iface, unsafe.Pointer(&buf[0])); err != nil {
		return 0, 0, fmt.Errorf("ETHTOOL_GSTRINGS: %w", err)
	}

	strs := buf[unsafe.Sizeof(ethtoolGstringsHdr{}):]
	for i := uint32(0); i < n; i++ {
		off := int(i) * ETH_GSTRING_LEN
		s := strs[off : off+ETH_GSTRING_LEN]
		if end := strings.IndexByte(string(s), 0); end >= 0 {
			s = s[:end]
		}
		if string(s) == name {
			featureIndexCacheMu.Lock()
			featureIndexCache[name] = featureIndexCacheEntry{idx: i, total: n}
			featureIndexCacheMu.Unlock()
			return i, n, nil
		}
	}
	return 0, n, fmt.Errorf("feature %q not found on %s", name, iface)
}

// setFeature flips a single feature bit on/off using ETHTOOL_SFEATURES.
func setFeature(fd int, iface string, idx, total uint32, on bool) error {
	// number of u32 blocks needed to cover `total` bits
	blocks := (total + 31) / 32

	// struct ethtool_sfeatures { __u32 cmd; __u32 size; struct ethtool_set_features_block features[]; }
	size := 8 + int(blocks)*int(unsafe.Sizeof(setFeatureBlock{}))
	buf := make([]byte, size)
	binary.LittleEndian.PutUint32(buf[0:4], unix.ETHTOOL_SFEATURES)
	binary.LittleEndian.PutUint32(buf[4:8], blocks)

	blockIdx := idx / 32
	bit := uint32(1) << (idx % 32)

	off := 8 + int(blockIdx)*int(unsafe.Sizeof(setFeatureBlock{}))
	// valid = bit (this bit will be touched)
	binary.LittleEndian.PutUint32(buf[off:off+4], bit)
	// requested = bit if on, else 0
	var req uint32
	if on {
		req = bit
	}
	binary.LittleEndian.PutUint32(buf[off+4:off+8], req)

	if err := ioctl(fd, iface, unsafe.Pointer(&buf[0])); err != nil {
		return fmt.Errorf("ETHTOOL_SFEATURES: %w", err)
	}
	return nil
}

func enableTXTCPMangleIDSegmentation(iface string) {
	logger := CubeLog.WithContext(context.Background())
	logger.Infof("network-agent ethtool enable %s begin: iface=%s", featNameTxTCPMangleIDSegmentation, iface)

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		logger.Warnf("network-agent ethtool enable %s open socket failed: iface=%s err=%v", featNameTxTCPMangleIDSegmentation, iface, err)
		return
	}
	defer unix.Close(fd)

	idx, total, err := featureIndex(fd, iface, featNameTxTCPMangleIDSegmentation)
	if err != nil {
		logger.Warnf("network-agent ethtool enable %s lookup feature failed: iface=%s err=%v", featNameTxTCPMangleIDSegmentation, iface, err)
		return
	}

	err = setFeature(fd, iface, idx, total, true)
	if err != nil {
		logger.Warnf("network-agent ethtool enable %s set feature failed: iface=%s idx=%d total=%d err=%v", featNameTxTCPMangleIDSegmentation, iface, idx, total, err)
		return
	}

	logger.Infof("network-agent ethtool enable %s done: iface=%s idx=%d total=%d", featNameTxTCPMangleIDSegmentation, iface, idx, total)
}
