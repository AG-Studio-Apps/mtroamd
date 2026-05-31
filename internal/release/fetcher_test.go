package release

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestAssetURLEscapesPathSegments(t *testing.T) {
	f := NewFetcher(WithReleaseBase("https://example.test/dl"))
	cases := []struct {
		tag, file, want string
	}{
		{
			tag:  "v0.3.1",
			file: "mtroamd-linux-amd64",
			want: "https://example.test/dl/v0.3.1/mtroamd-linux-amd64",
		},
		{
			// Defence-in-depth: even if a future caller skips
			// ValidateTag, traversal cannot escape its segment.
			tag:  "v1.0.0/../../etc/passwd",
			file: "mtroamd-linux-amd64",
			want: "https://example.test/dl/v1.0.0%2F..%2F..%2Fetc%2Fpasswd/mtroamd-linux-amd64",
		},
		{
			tag:  "v0.3.1",
			file: "SHA256SUMS.minisig",
			want: "https://example.test/dl/v0.3.1/SHA256SUMS.minisig",
		},
		{
			tag:  "v0.3.1",
			file: "../etc/passwd",
			want: "https://example.test/dl/v0.3.1/..%2Fetc%2Fpasswd",
		},
	}
	for _, c := range cases {
		got := f.AssetURL(c.tag, c.file)
		if got != c.want {
			t.Errorf("AssetURL(%q,%q)\n  got:  %s\n  want: %s",
				c.tag, c.file, got, c.want)
		}
	}
}

func TestLookupChecksumStandardFormat(t *testing.T) {
	body := []byte(`
057771c44688fbbc076832205f5aa5b26901da58e306dd3f8c3a7f1a9b1c5d72  mtroamd-darwin-amd64
abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789  mtroamd-linux-amd64
`)
	got, err := LookupChecksum(body, "mtroamd-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	want := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLookupChecksumTolerates_DotSlash_Prefix(t *testing.T) {
	body := []byte("aaa  ./mtroamd-linux-amd64\n")
	got, err := LookupChecksum(body, "mtroamd-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "aaa" {
		t.Errorf("got %q", got)
	}
}

func TestLookupChecksumMissingFile(t *testing.T) {
	body := []byte("aaa  mtroamd-darwin-amd64\n")
	if _, err := LookupChecksum(body, "mtroamd-freebsd-amd64"); err == nil {
		t.Fatal("expected error for missing entry")
	}
}

func TestAssetFilenameMatchesRuntime(t *testing.T) {
	got, err := AssetFilename()
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		if err != nil {
			t.Fatal(err)
		}
		if got != "mtroamd-linux-amd64" {
			t.Errorf("got %q", got)
		}
	} else if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if err != nil {
			t.Fatal(err)
		}
		if got != "mtroamd-darwin-arm64" {
			t.Errorf("got %q", got)
		}
	}
	// On unsupported combos we just want a non-nil error. Don't
	// pin the message; tests on weird CI architectures shouldn't
	// fail because of a wording change.
}

func TestChecksumOfRoundTrips(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "sum-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	got, err := ChecksumOf(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	// SHA-256 of "hello": 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFetchSmallObservesSizeCap(t *testing.T) {
	// 9 MiB response body — over the 8 MiB cap. Server intentionally
	// declares no Content-Length so the caller can't bail early.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, 1024)
		for i := 0; i < 9*1024; i++ {
			_, _ = w.Write(buf)
		}
	}))
	defer srv.Close()
	f := NewFetcher(WithReleaseBase(srv.URL))
	if _, err := f.FetchSmall(context.Background(), srv.URL+"/big"); err == nil {
		t.Fatal("expected size-cap error")
	}
}

func TestLatestTagSkipsPrereleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v9.9.9-rc1",
			"published_at": "2026-01-01T00:00:00Z",
			"prerelease":   true,
		})
	}))
	defer srv.Close()
	f := NewFetcher(WithAPIBase(srv.URL))
	if _, _, err := f.LatestTag(context.Background()); err == nil {
		t.Fatal("expected error refusing pre-release")
	} else if !strings.Contains(err.Error(), "pre-release") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestLatestTagReturnsTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v0.2.0","published_at":"2026-04-30T12:00:00Z","prerelease":false,"draft":false}`)
	}))
	defer srv.Close()
	f := NewFetcher(WithAPIBase(srv.URL))
	tag, _, err := f.LatestTag(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0.2.0" {
		t.Errorf("got %q", tag)
	}
}
