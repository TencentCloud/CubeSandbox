// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

func SafeCopyFile(dst, src string) (err error) {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstNewFile := dst + ".new"                                                        // NOCC:Path Traversal()
	dstFile, err := os.OpenFile(dstNewFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644) // NOCC:Path Traversal()
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}
	return os.Rename(dstNewFile, dst)
}

const (
	SuperblockOffset = 1024
)

type Superblock struct {
	InodesCount     uint32
	BlocksCount     uint32
	RBlocksCount    uint32
	FreeBlocksCount uint32
	FreeInodesCount uint32
	FirstDataBlock  uint32
	LogBlockSize    uint32
	LogFragSize     uint32
	BlocksPerGroup  uint32
	FragsPerGroup   uint32
	InodesPerGroup  uint32
	Mtime           uint32
	Wtime           uint32
	MntCount        uint16
	MaxMntCount     uint16
	Magic           uint16
}

type BlockGroupDescriptor struct {
	BlockBitmap       uint32
	InodeBitmap       uint32
	InodeTable        uint32
	FreeBlocksCount   uint16
	FreeInodesCount   uint16
	UsedDirsCount     uint16
	Flags             uint16
	ExcludeBitmapLo   uint32
	BlockBitmapCsumLo uint16
	InodeBitmapCsumLo uint16
	ItableUnused      uint16
	Checksum          uint16
}

func GetExt4BlockGroupDescriptor(device string) (*BlockGroupDescriptor, error) {
	file, err := os.Open(device)
	if err != nil {
		return nil, fmt.Errorf("failed to open device: %w", err)
	}
	defer file.Close()

	var sb Superblock
	if _, err = file.Seek(SuperblockOffset, 0); err != nil {
		return nil, fmt.Errorf("failed to seek to superblock: %w", err)
	}
	if err = binary.Read(file, binary.LittleEndian, &sb); err != nil {
		return nil, fmt.Errorf("failed to read superblock: %w", err)
	}

	if sb.Magic != 0xEF53 {
		return nil, fmt.Errorf("not a valid ext2/ext3/ext4 filesystem")
	}

	blockSize := 1024 << sb.LogBlockSize
	groupDescriptorOffset := blockSize
	if blockSize <= 2048 {
		groupDescriptorOffset = 2048
	}

	var gd BlockGroupDescriptor
	if _, err = file.Seek(int64(groupDescriptorOffset), 0); err != nil {
		return nil, fmt.Errorf("failed to seek to group descriptor: %w", err)
	}
	if err = binary.Read(file, binary.LittleEndian, &gd); err != nil {
		return nil, fmt.Errorf("failed to read group descriptor: %w", err)
	}

	return &gd, nil
}
