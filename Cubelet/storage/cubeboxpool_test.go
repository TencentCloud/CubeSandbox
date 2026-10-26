// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
)

func TestInitBaseFile_NoVersionToVersion(t *testing.T) {

	_, err := config.Init("", true)
	if err != nil {
		t.Fatalf("failed to init config: %v", err)
	}

	config.GetCommon().DisableCubeBoxTemplateBaseFormatPoolOfNumberVer = false

	tmpDir, err := os.MkdirTemp("", "cubebox_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	templateID := "test_template"
	baseFormatPath := filepath.Join(tmpDir, templateID)
	err = os.MkdirAll(baseFormatPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	content := make([]byte, 2048)
	baseFile := filepath.Join(baseFormatPath, baseFileName)
	err = os.WriteFile(baseFile, content, 0644)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		dir := filepath.Join(baseFormatPath, strconv.Itoa(i))
		err = os.MkdirAll(dir, 0755)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(dir, baseFileName), content, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	p := &cubeboxWithReflink{
		baseFormatPath: baseFormatPath,
		baseNum:        3,
		prefetchBlocks: []uint32{0},
	}

	ctx := context.Background()
	err = p.InitBaseFile(ctx)
	assert.NoError(t, err)

	time.Sleep(200 * time.Millisecond)

	currentLink := filepath.Join(baseFormatPath, "current")
	target, err := os.Readlink(currentLink)
	assert.NoError(t, err)
	assert.Contains(t, target, "ver_")

	newVersionPath := filepath.Join(baseFormatPath, target)
	assert.DirExists(t, newVersionPath)

	for i := 0; i < 3; i++ {
		dir := filepath.Join(baseFormatPath, strconv.Itoa(i))
		assert.DirExists(t, dir)
	}
}

func TestInitBaseFile_VersionToVersion(t *testing.T) {

	_, err := config.Init("", true)
	if err != nil {
		t.Fatalf("failed to init config: %v", err)
	}
	config.GetCommon().DisableCubeBoxTemplateBaseFormatPoolOfNumberVer = false

	tmpDir, err := os.MkdirTemp("", "cubebox_test_v2v")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	templateID := "test_template"
	baseFormatPath := filepath.Join(tmpDir, templateID)
	err = os.MkdirAll(baseFormatPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	content := make([]byte, 2048)
	baseFile := filepath.Join(baseFormatPath, baseFileName)
	err = os.WriteFile(baseFile, content, 0644)
	if err != nil {
		t.Fatal(err)
	}

	oldVersion := "ver_old"
	oldVersionPath := filepath.Join(baseFormatPath, oldVersion)
	err = os.MkdirAll(oldVersionPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(oldVersion, filepath.Join(baseFormatPath, "current"))
	if err != nil {
		t.Fatal(err)
	}

	p := &cubeboxWithReflink{
		baseFormatPath: baseFormatPath,
		baseNum:        3,
		prefetchBlocks: []uint32{0},
	}

	ctx := context.Background()
	err = p.InitBaseFile(ctx)
	assert.NoError(t, err)

	time.Sleep(200 * time.Millisecond)

	currentLink := filepath.Join(baseFormatPath, "current")
	target, err := os.Readlink(currentLink)
	assert.NoError(t, err)
	assert.Contains(t, target, "ver_")
	assert.NotEqual(t, oldVersion, target)

	newVersionPath := filepath.Join(baseFormatPath, target)
	assert.DirExists(t, newVersionPath)

	assert.NoDirExists(t, oldVersionPath)
}

func TestInitBaseFile_EnsureLegacyUpdate(t *testing.T) {

	_, err := config.Init("", true)
	if err != nil {
		t.Fatalf("failed to init config: %v", err)
	}
	config.GetCommon().DisableCubeBoxTemplateBaseFormatPoolOfNumberVer = false

	tmpDir, err := os.MkdirTemp("", "cubebox_test_legacy_update")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	templateID := "test_template_legacy"
	baseFormatPath := filepath.Join(tmpDir, templateID)
	err = os.MkdirAll(baseFormatPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	newContent := make([]byte, 2048)
	for i := range newContent {
		newContent[i] = 'n'
	}
	baseFile := filepath.Join(baseFormatPath, baseFileName)
	err = os.WriteFile(baseFile, newContent, 0644)
	if err != nil {
		t.Fatal(err)
	}

	oldContent := make([]byte, 2048)
	for i := range oldContent {
		oldContent[i] = 'o'
	}
	for i := 0; i < 3; i++ {
		dir := filepath.Join(baseFormatPath, strconv.Itoa(i))
		err = os.MkdirAll(dir, 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(filepath.Join(dir, baseFileName), oldContent, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	p := &cubeboxWithReflink{
		baseFormatPath: baseFormatPath,
		baseNum:        3,
		prefetchBlocks: []uint32{0},
	}

	ctx := context.Background()
	err = p.InitBaseFile(ctx)
	assert.NoError(t, err)

	for i := 0; i < 3; i++ {
		legacyFile := filepath.Join(baseFormatPath, strconv.Itoa(i), baseFileName)
		content, err := os.ReadFile(legacyFile)
		assert.NoError(t, err)
		assert.Equal(t, newContent, content, "Legacy file should be updated to new content")
	}
}

func TestInitBaseFile_DisableLegacyCompatibility(t *testing.T) {

	_, err := config.Init("", true)
	if err != nil {
		t.Fatalf("failed to init config: %v", err)
	}

	config.GetCommon().DisableCubeBoxTemplateBaseFormatPoolOfNumberVer = true

	tmpDir, err := os.MkdirTemp("", "cubebox_test_disable_legacy")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	templateID := "test_template_disable_legacy"
	baseFormatPath := filepath.Join(tmpDir, templateID)
	err = os.MkdirAll(baseFormatPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	content := make([]byte, 2048)
	baseFile := filepath.Join(baseFormatPath, baseFileName)
	err = os.WriteFile(baseFile, content, 0644)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		dir := filepath.Join(baseFormatPath, strconv.Itoa(i))
		err = os.MkdirAll(dir, 0755)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(dir, baseFileName), content, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	p := &cubeboxWithReflink{
		baseFormatPath: baseFormatPath,
		baseNum:        3,
		prefetchBlocks: []uint32{0},
	}

	ctx := context.Background()
	err = p.InitBaseFile(ctx)
	assert.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	currentLink := filepath.Join(baseFormatPath, "current")
	target, err := os.Readlink(currentLink)
	assert.NoError(t, err)
	assert.Contains(t, target, "ver_")

	newVersionPath := filepath.Join(baseFormatPath, target)
	assert.DirExists(t, newVersionPath)

	for i := 0; i < 3; i++ {
		dir := filepath.Join(baseFormatPath, strconv.Itoa(i))
		assert.NoDirExists(t, dir, "Legacy directory should be removed when DisableCubeBoxTemplateBaseFormatPoolOfNumberVer is true")
	}
}
