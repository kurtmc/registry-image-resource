package resource_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resource "github.com/concourse/registry-image-resource"
)

var _ = Describe("Windows images", func() {
	var (
		host    string
		workDir string
	)

	user := "ContainerUser"
	env := []string{"FOO=1", "BAR=2"}

	windows := &resource.PlatformField{OS: "windows", Architecture: "amd64"}
	linux := &resource.PlatformField{OS: "linux", Architecture: "amd64"}

	newRegistry := func() string {
		server := httptest.NewServer(registry.New(registry.Logger(log.New(GinkgoWriter, "", 0))))
		DeferCleanup(server.Close)

		return strings.TrimPrefix(server.URL, "http://")
	}

	BeforeEach(func() {
		host = newRegistry()

		var err error
		workDir, err = os.MkdirTemp("", "windows-image")
		Expect(err).ToNot(HaveOccurred())

		DeferCleanup(os.RemoveAll, workDir)
	})

	image := func(goos string) v1.Image {
		base, err := random.Image(1024, 2)
		Expect(err).ToNot(HaveOccurred())

		cfg, err := base.ConfigFile()
		Expect(err).ToNot(HaveOccurred())

		cfg.OS = goos
		cfg.Architecture = "amd64"
		cfg.Config.User = user
		cfg.Config.Env = env

		img, err := mutate.ConfigFile(base, cfg)
		Expect(err).ToNot(HaveOccurred())

		return img
	}

	digestOf := func(d interface{ Digest() (v1.Hash, error) }) string {
		digest, err := d.Digest()
		Expect(err).ToNot(HaveOccurred())

		return digest.String()
	}

	tag := func(repository string) name.Tag {
		t, err := name.NewTag(repository + ":latest")
		Expect(err).ToNot(HaveOccurred())

		return t
	}

	source := func(repository string, platform *resource.PlatformField) resource.Source {
		return resource.Source{
			Repository:  repository,
			Tag:         "latest",
			RawPlatform: platform,
		}
	}

	run := func(bin string, req any, args ...string) []byte {
		payload, err := json.Marshal(req)
		Expect(err).ToNot(HaveOccurred())

		stdout := new(bytes.Buffer)

		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"TEST=true"}
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = stdout
		cmd.Stderr = GinkgoWriter

		Expect(cmd.Run()).To(Succeed())

		return stdout.Bytes()
	}

	get := func(src resource.Source, version resource.Version) string {
		dest, err := os.MkdirTemp(workDir, "get")
		Expect(err).ToNot(HaveOccurred())

		run(bins.In, resource.InRequest{Source: src, Version: version}, dest)

		return dest
	}

	layoutDigests := func(dir string) []string {
		idx, err := layout.ImageIndexFromPath(dir)
		Expect(err).ToNot(HaveOccurred())

		manifest, err := idx.IndexManifest()
		Expect(err).ToNot(HaveOccurred())

		digests := []string{}
		for _, desc := range manifest.Manifests {
			digests = append(digests, desc.Digest.String())
		}

		return digests
	}

	dirNames := func(dir string) []string {
		entries, err := os.ReadDir(dir)
		Expect(err).ToNot(HaveOccurred())

		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		return names
	}

	Describe("a single Windows image", func() {
		var (
			repository string
			img        v1.Image
			imgDigest  string
			dest       string
		)

		BeforeEach(func() {
			repository = host + "/windows-image"

			img = image("windows")
			imgDigest = digestOf(img)
			Expect(remote.Write(tag(repository), img)).To(Succeed())

			dest = get(source(repository, windows), resource.Version{Tag: "latest", Digest: imgDigest})
		})

		It("is fetched as an OCI layout in rootfs", func() {
			Expect(layoutDigests(filepath.Join(dest, "rootfs"))).To(Equal([]string{imgDigest}))
			Expect(cat(filepath.Join(dest, "rootfs", "single-image-digest"))).To(Equal(imgDigest))
		})

		It("keeps the layer blobs as fetched", func() {
			layers, err := img.Layers()
			Expect(err).ToNot(HaveOccurred())
			Expect(layers).ToNot(BeEmpty())

			for _, layer := range layers {
				layerDigest, err := layer.Digest()
				Expect(err).ToNot(HaveOccurred())

				blob, err := os.ReadFile(filepath.Join(dest, "rootfs", "blobs", layerDigest.Algorithm, layerDigest.Hex))
				Expect(err).ToNot(HaveOccurred())

				sum := sha256.Sum256(blob)
				Expect(hex.EncodeToString(sum[:])).To(Equal(layerDigest.Hex))
			}
		})

		It("is not unpacked into rootfs", func() {
			Expect(dirNames(filepath.Join(dest, "rootfs"))).To(ConsistOf("oci-layout", "index.json", "blobs", "single-image-digest"))
		})

		It("writes the env and user to metadata.json", func() {
			var meta struct {
				User string   `json:"user"`
				Env  []string `json:"env"`
			}

			Expect(json.Unmarshal([]byte(cat(filepath.Join(dest, "metadata.json"))), &meta)).To(Succeed())
			Expect(meta.User).To(Equal(user))
			Expect(meta.Env).To(Equal(env))
		})

		Context("when the layout is pushed back with out", func() {
			It("pushes the same image to an empty registry, which is fetched as the same layout again", func() {
				copyRepository := newRegistry() + "/windows-image"

				var pushed resource.OutResponse

				out := run(bins.Out, resource.OutRequest{
					Source: source(copyRepository, windows),
					Params: resource.PutParams{Image: filepath.Join(filepath.Base(dest), "rootfs")},
				}, workDir)
				Expect(json.Unmarshal(out, &pushed)).To(Succeed())
				Expect(pushed.Version.Digest).To(Equal(imgDigest))

				desc, err := remote.Head(tag(copyRepository))
				Expect(err).ToNot(HaveOccurred())
				Expect(desc.Digest.String()).To(Equal(imgDigest))

				again := get(source(copyRepository, windows), pushed.Version)
				Expect(layoutDigests(filepath.Join(again, "rootfs"))).To(Equal([]string{imgDigest}))
			})
		})
	})

	Describe("a multi-platform index", func() {
		var (
			repository string
			linuxImg   v1.Image
			windowsImg v1.Image
			version    resource.Version
		)

		BeforeEach(func() {
			repository = host + "/mixed"

			linuxImg = image("linux")
			windowsImg = image("windows")

			idx := mutate.AppendManifests(empty.Index,
				mutate.IndexAddendum{
					Add:        linuxImg,
					Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
				},
				mutate.IndexAddendum{
					Add:        windowsImg,
					Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "windows", Architecture: "amd64"}},
				},
			)
			Expect(remote.WriteIndex(tag(repository), idx)).To(Succeed())

			version = resource.Version{Tag: "latest", Digest: digestOf(idx)}
		})

		It("is fetched as the Windows image's OCI layout for windows/amd64", func() {
			dest := get(source(repository, windows), version)

			Expect(layoutDigests(filepath.Join(dest, "rootfs"))).To(Equal([]string{digestOf(windowsImg)}))
		})

		It("is fetched unpacked from the Linux image for linux/amd64", func() {
			dest := get(source(repository, linux), version)

			layers, err := linuxImg.Layers()
			Expect(err).ToNot(HaveOccurred())
			Expect(layers).ToNot(BeEmpty())

			for _, layer := range layers {
				content, err := layer.Uncompressed()
				Expect(err).ToNot(HaveOccurred())

				hdr, err := tar.NewReader(content).Next()
				Expect(err).ToNot(HaveOccurred())
				Expect(content.Close()).To(Succeed())

				info, err := os.Stat(filepath.Join(dest, "rootfs", hdr.Name))
				Expect(err).ToNot(HaveOccurred())
				Expect(info.Size()).To(Equal(hdr.Size))
			}

			Expect(filepath.Join(dest, "rootfs", "index.json")).ToNot(BeAnExistingFile())
			Expect(filepath.Join(dest, "rootfs", "oci-layout")).ToNot(BeAnExistingFile())
		})

		It("is checked as the digest of the index for windows/amd64", func() {
			var versions []resource.Version

			out := run(bins.Check, resource.CheckRequest{Source: source(repository, windows)})
			Expect(json.Unmarshal(out, &versions)).To(Succeed())
			Expect(versions).To(Equal([]resource.Version{version}))
		})
	})
})
