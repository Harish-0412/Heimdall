// Package bundle transports configuration and approved fixture files through
// the customer's OCI registry. Contents are never sent to the control plane.
package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/render"
)

const (
	ArtifactType     = "application/vnd.heimdall.bundle.v1"
	ConfigType       = "application/vnd.heimdall.config.v1+yaml"
	DataType         = "application/vnd.heimdall.data.v1+sql"
	MaxConfigBytes   = 256 << 10
	MaxManifestBytes = 16 << 10
	LayoutTag        = "bundle"
)

type Contents struct {
	Config   []byte
	Seed     []byte
	SeedPath string
}

// ConfigDigest binds exactly the bytes GitHub, CI, the API and agent observe.
func ConfigDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validPath(name string) bool {
	return name != "" && name == path.Clean(name) && !path.IsAbs(name) &&
		!strings.ContainsAny(name, "\\:\x00") && name != ".." && !strings.HasPrefix(name, "../")
}

func readFile(root *os.Root, name string, limit int64) ([]byte, error) {
	if !validPath(name) {
		return nil, errors.New("bundle path must be a clean repository-relative path")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("bundle file is not regular or exceeds its size limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, errors.New("bundle file exceeds its limit or is not plain UTF-8 text")
	}
	return b, nil
}

// Pack creates a reproducible OCI layout. It reads only the config and its
// explicitly declared SQL import, using os.Root to contain symlinks.
func Pack(ctx context.Context, repositoryRoot, configPath, layout string) (ocispec.Descriptor, error) {
	r, err := os.OpenRoot(repositoryRoot)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer r.Close()
	raw, err := readFile(r, configPath, MaxConfigBytes)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	cfg, diags := config.Load(bytes.NewReader(raw), config.BaselinePolicy())
	if cfg == nil {
		return ocispec.Descriptor{}, fmt.Errorf("invalid bundle config: %v", diags)
	}
	s, err := oci.New(layout)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	layers := []ocispec.Descriptor{}
	add := func(mediaType, name string, b []byte) error {
		d := content.NewDescriptorFromBytes(mediaType, b)
		d.Annotations = map[string]string{ocispec.AnnotationTitle: name}
		if err := s.Push(ctx, d, bytes.NewReader(b)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return err
		}
		layers = append(layers, d)
		return nil
	}
	if err := add(ConfigType, "heimdall.yaml", raw); err != nil {
		return ocispec.Descriptor{}, err
	}
	if p := cfg.Dependencies.Postgres; p != nil && p.Seed != "" {
		seed, err := readFile(r, p.Seed, render.MaxSeedBytes)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		if err := add(DataType, p.Seed, seed); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	d, err := oras.PackManifest(ctx, s, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers: layers, ManifestAnnotations: map[string]string{ocispec.AnnotationCreated: "1970-01-01T00:00:00Z"},
	})
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := s.Tag(ctx, d, LayoutTag); err != nil {
		return ocispec.Descriptor{}, err
	}
	return d, nil
}

type RegistryOptions struct {
	// PlainHTTP is an explicit local-test setting, never inferred from a PR.
	PlainHTTP    bool
	DockerConfig string
	Credential   auth.CredentialFunc
}

func repository(reference string, opts RegistryOptions) (*remote.Repository, registry.Reference, error) {
	r, err := registry.ParseReference(reference)
	if err != nil {
		return nil, r, err
	}
	p, err := remote.NewRepository(r.Registry + "/" + r.Repository)
	if err != nil {
		return nil, r, err
	}
	p.PlainHTTP = opts.PlainHTTP
	get := opts.Credential
	if get == nil {
		var store credentials.Store
		if opts.DockerConfig != "" {
			store, err = credentials.NewStore(opts.DockerConfig, credentials.StoreOptions{})
		} else {
			store, err = credentials.NewStoreFromDocker(credentials.StoreOptions{})
		}
		if err != nil {
			return nil, r, err
		}
		get = credentials.Credential(store)
		dockerGet := get
		get = func(ctx context.Context, host string) (auth.Credential, error) {
			if IsECR(host) {
				return ECRCredential(ctx, host)
			}
			return dockerGet(ctx, host)
		}
	}
	p.Client = &auth.Client{Client: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Cache: auth.NewCache(), Credential: get}
	return p, r, nil
}

// Push publishes a layout and returns its immutable digest reference.
func Push(ctx context.Context, layout, reference string, opts RegistryOptions) (string, error) {
	s, err := oci.New(layout)
	if err != nil {
		return "", err
	}
	dst, ref, err := repository(reference, opts)
	if err != nil {
		return "", err
	}
	if ref.Reference == "" {
		return "", errors.New("bundle push requires an explicit tag or digest")
	}
	d, err := oras.Copy(ctx, s, LayoutTag, dst, ref.Reference, oras.DefaultCopyOptions)
	if err != nil {
		return "", err
	}
	return ref.Registry + "/" + ref.Repository + "@" + d.Digest.String(), nil
}

func readDescriptor(ctx context.Context, f content.Fetcher, d ocispec.Descriptor, limit int64) ([]byte, error) {
	if d.Size < 0 || d.Size > limit || d.Digest.Algorithm() != digest.SHA256 || d.Digest.Validate() != nil {
		return nil, errors.New("invalid or oversized bundle descriptor")
	}
	r, err := f.Fetch(ctx, d)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != d.Size || digest.FromBytes(b) != d.Digest {
		return nil, errors.New("bundle content does not match its descriptor")
	}
	return b, nil
}

// Fetch accepts only digest-pinned artifacts. It validates sizes and all hashes
// before exposing config or data, and never extracts arbitrary archive paths.
func Fetch(ctx context.Context, reference, expectedConfigDigest string, opts RegistryOptions) (*Contents, error) {
	repo, ref, err := repository(reference, opts)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ref.Reference, "sha256:") || digest.Digest(ref.Reference).Validate() != nil {
		return nil, errors.New("bundle reference must be pinned by SHA-256 digest")
	}
	d, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return nil, err
	}
	if d.Digest.String() != ref.Reference || d.MediaType != ocispec.MediaTypeImageManifest {
		return nil, errors.New("unexpected bundle manifest")
	}
	b, err := readDescriptor(ctx, repo, d, MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.SchemaVersion != 2 || m.ArtifactType != ArtifactType || len(m.Layers) < 1 || len(m.Layers) > 2 || m.Subject != nil {
		return nil, errors.New("unsupported bundle format")
	}
	if len(m.Config.URLs) != 0 || (len(m.Config.Data) != 0 && string(m.Config.Data) != "{}") || m.Config.MediaType != ocispec.MediaTypeEmptyJSON {
		return nil, errors.New("unexpected bundle configuration descriptor")
	}
	empty, err := readDescriptor(ctx, repo, m.Config, 2)
	if err != nil || string(empty) != "{}" {
		return nil, errors.New("invalid empty bundle configuration")
	}
	out := &Contents{}
	for _, layer := range m.Layers {
		if len(layer.URLs) != 0 || len(layer.Data) != 0 {
			return nil, errors.New("external bundle layer URLs and embedded content are forbidden")
		}
		name := layer.Annotations[ocispec.AnnotationTitle]
		if !validPath(name) {
			return nil, errors.New("unsafe bundle layer path")
		}
		switch layer.MediaType {
		case ConfigType:
			if out.Config != nil || name != "heimdall.yaml" {
				return nil, errors.New("duplicate or invalid config layer")
			}
			out.Config, err = readDescriptor(ctx, repo, layer, MaxConfigBytes)
		case DataType:
			if out.Seed != nil || !strings.HasSuffix(strings.ToLower(name), ".sql") {
				return nil, errors.New("duplicate or invalid data layer")
			}
			out.Seed, err = readDescriptor(ctx, repo, layer, render.MaxSeedBytes)
			out.SeedPath = name
		default:
			return nil, errors.New("unexpected bundle layer type")
		}
		if err != nil {
			return nil, err
		}
	}
	if len(out.Config) == 0 || ConfigDigest(out.Config) != expectedConfigDigest || !utf8.Valid(out.Config) {
		return nil, errors.New("bundle config does not match the approved configuration digest")
	}
	cfg, diags := config.Load(bytes.NewReader(out.Config), config.BaselinePolicy())
	if cfg == nil {
		return nil, fmt.Errorf("invalid bundle config: %v", diags)
	}
	seedPath := ""
	if p := cfg.Dependencies.Postgres; p != nil {
		seedPath = p.Seed
	}
	if seedPath != out.SeedPath || (out.Seed != nil && (!utf8.Valid(out.Seed) || bytes.IndexByte(out.Seed, 0) >= 0)) {
		return nil, errors.New("bundle data does not match the declared import")
	}
	return out, nil
}
