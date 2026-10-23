package images

import "fmt"

// IsPmemStorageMedia identifies immutable filesystem artifacts. Delivery policy
// is separate from this filesystem discriminator.
func IsPmemStorageMedia(media string) bool {
	// Empty storage_media belongs to the ordinary OCI path. Only resolve the
	// legacy default after a caller has established that this is a rootfs artifact.
	if media == "" {
		return false
	}
	_, err := ResolveRootfsFormat(media)
	return err == nil
}

// ResolveRootfsFormat decodes an immutable PMEM filesystem format. Missing
// values belong to historical EXT4 artifacts, not ordinary OCI images.
func ResolveRootfsFormat(format string) (string, error) {
	switch format {
	case "", ImageStorageMediaType_ext4.String():
		return ImageStorageMediaType_ext4.String(), nil
	default:
		return "", fmt.Errorf("unsupported rootfs format %q", format)
	}
}

// RootfsFilename names a materialized filesystem, independently of its download
// URL or object-store location. Callers validate the artifact ID at their input.
func RootfsFilename(artifactID, format string) (string, error) {
	resolved, err := ResolveRootfsFormat(format)
	if err != nil {
		return "", err
	}
	return artifactID + "." + resolved, nil
}
