package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const testConfig = "version: 1\nservices:\n  web:\n    build: {context: .}\n    port: 3000\n"

func TestBundleSyntaxDoesNotImposeLocalDeploymentLimits(t *testing.T) {
	// A tenant may explicitly authorize these resources. Artifact transport
	// validates bounded syntax; deployment policy is enforced on receipt.
	document := testConfig + "    resources: {cpu: 4000m, memory: 8192Mi}\n"
	root, layout := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "heimdall.yaml"), []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), root, "heimdall.yaml", layout); err != nil {
		t.Fatalf("authorized custom-limit syntax rejected before policy evaluation: %v", err)
	}
}

func TestPackReproducibleAndContainsOnlyDeclaredFiles(t *testing.T) {
	root, layout := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "heimdall.yaml"), []byte(testConfig), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "private.key"), []byte("never bundle this"), 0600)
	a, err := Pack(context.Background(), root, "heimdall.yaml", layout)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Pack(context.Background(), root, "heimdall.yaml", layout)
	if err != nil || a.Digest != b.Digest {
		t.Fatalf("bundle changed on repeat: %v", err)
	}
	s, _ := oci.New(layout)
	r, _ := s.Fetch(context.Background(), a)
	raw, _ := io.ReadAll(r)
	_ = r.Close()
	var m ocispec.Manifest
	_ = json.Unmarshal(raw, &m)
	if len(m.Layers) != 1 || m.Layers[0].Annotations[ocispec.AnnotationTitle] != "heimdall.yaml" {
		t.Fatal("unexpected repository files included")
	}
	if _, err := Pack(context.Background(), root, "../outside", t.TempDir()); err == nil {
		t.Fatal("path escape accepted")
	}
}

func TestFetchRejectsTagsUnapprovedConfigAndExternalLayers(t *testing.T) {
	ctx := context.Background()
	root, layout := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "heimdall.yaml"), []byte(testConfig), 0600)
	desc, err := Pack(ctx, root, "heimdall.yaml", layout)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := oci.New(layout)
	read := func(d ocispec.Descriptor) []byte {
		r, err := s.Fetch(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return b
	}
	manifest := read(desc)
	var m ocispec.Manifest
	_ = json.Unmarshal(manifest, &m)
	blobs := map[string][]byte{m.Config.Digest.String(): read(m.Config)}
	for _, l := range m.Layers {
		blobs[l.Digest.String()] = read(l)
	}
	serve := func(manifest []byte) (*httptest.Server, string) {
		sha := digest.FromBytes(manifest).String()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/" {
				w.WriteHeader(200)
				return
			}
			if strings.Contains(r.URL.Path, "/manifests/") {
				w.Header().Set("Docker-Content-Digest", sha)
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
				if r.Method != "HEAD" {
					_, _ = w.Write(manifest)
				}
				return
			}
			for id, b := range blobs {
				if strings.HasSuffix(r.URL.Path, id) {
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(b)
					return
				}
			}
			http.NotFound(w, r)
		}))
		return server, strings.TrimPrefix(server.URL, "http://") + "/bundle@" + sha
	}
	server, ref := serve(manifest)
	defer server.Close()
	opts := RegistryOptions{PlainHTTP: true, Credential: func(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }}
	b, err := Fetch(ctx, ref, ConfigDigest([]byte(testConfig)), opts)
	if err != nil || !bytes.Equal(b.Config, []byte(testConfig)) {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if _, err := Fetch(ctx, ref, strings.Repeat("0", 64), opts); err == nil {
		t.Fatal("unapproved config accepted")
	}
	if _, err := Fetch(ctx, strings.Split(ref, "@")[0]+":latest", ConfigDigest([]byte(testConfig)), opts); err == nil {
		t.Fatal("mutable tag accepted")
	}
	m.Layers[0].URLs = []string{"https://attacker.test/secret"}
	changed, _ := json.Marshal(m)
	bad, badref := serve(changed)
	defer bad.Close()
	if _, err := Fetch(ctx, badref, ConfigDigest([]byte(testConfig)), opts); err == nil {
		t.Fatal("external layer URL accepted")
	}
}
