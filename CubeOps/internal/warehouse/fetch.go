// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	SourceGitHub = "github"
	SourceCNB    = "cnb"
	SourceUpload = "upload"

	oneClickAssetFmt = "cube-sandbox-one-click-%s-%s.tar.gz"
)

// FetchConfig is remote-import policy (whitelist + optional tokens).
type FetchConfig struct {
	GitHubRepos []string
	CNBRepos    []string
	GitHubToken string
	CNBToken    string
	Timeout     time.Duration
}

func (c FetchConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 30 * time.Minute
}

func allowedRepo(list []string, repo string) bool {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "..") || strings.ContainsAny(repo, " \t\n\r") {
		return false
	}
	// owner/name, optionally deeper for CNB (group/sub/name)
	if strings.Count(repo, "/") < 1 {
		return false
	}
	for _, allowed := range list {
		if strings.EqualFold(strings.TrimSpace(allowed), repo) {
			return true
		}
	}
	return false
}

func oneClickAssetName(tag, arch string) string {
	return fmt.Sprintf(oneClickAssetFmt, tag, arch)
}

func githubReleaseURL(repo, tag, arch string) string {
	asset := oneClickAssetName(tag, arch)
	return "https://github.com/" + repo + "/releases/download/" + url.PathEscape(tag) + "/" + asset
}

func cnbReleaseURL(repo, tag, arch string) string {
	asset := oneClickAssetName(tag, arch)
	return "https://cnb.cool/" + repo + "/-/releases/download/" + url.PathEscape(tag) + "/" + asset
}

func allowedDownloadHost(host string) bool {
	host = strings.ToLower(host)
	switch {
	case host == "github.com":
		return true
	case strings.HasSuffix(host, ".githubusercontent.com"):
		return true
	case host == "cnb.cool":
		return true
	case strings.HasSuffix(host, ".cnb.cool"):
		return true
	default:
		return false
	}
}

func (c FetchConfig) httpClient() *http.Client {
	return &http.Client{
		Timeout: c.timeout(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if !allowedDownloadHost(req.URL.Hostname()) {
				return fmt.Errorf("redirect to disallowed host %s", req.URL.Hostname())
			}
			return nil
		},
	}
}

// DownloadRelease fetches a one-click asset into destPath.
func (c FetchConfig) DownloadRelease(source, repo, tag, arch, destPath string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.Contains(tag, "..") || strings.ContainsAny(tag, `/\`) {
		return fmt.Errorf("invalid tag")
	}
	arch, err := NormalizeArch(arch)
	if err != nil {
		return err
	}

	var rawURL, token, tokenHeader string
	switch source {
	case SourceGitHub:
		if !allowedRepo(c.GitHubRepos, repo) {
			return fmt.Errorf("github repo %q is not in the warehouse whitelist", repo)
		}
		rawURL = githubReleaseURL(repo, tag, arch)
		token = c.GitHubToken
		tokenHeader = "Bearer " + token
	case SourceCNB:
		if !allowedRepo(c.CNBRepos, repo) {
			return fmt.Errorf("cnb repo %q is not in the warehouse whitelist", repo)
		}
		rawURL = cnbReleaseURL(repo, tag, arch)
		token = c.CNBToken
		tokenHeader = "Bearer " + token
	default:
		return fmt.Errorf("unsupported import source %q", source)
	}

	u, err := url.Parse(rawURL)
	if err != nil || !allowedDownloadHost(u.Hostname()) {
		return fmt.Errorf("refusing download URL")
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", tokenHeader)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", path.Base(rawURL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", path.Base(rawURL), resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), dirPerm); err != nil {
		return err
	}
	tmp := destPath + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, destPath)
}
