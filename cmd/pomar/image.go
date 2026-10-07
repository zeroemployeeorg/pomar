package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/ocilayout"
	"github.com/zeroemployeeorg/pomar/internal/sign"
	"github.com/zeroemployeeorg/pomar/internal/smoke"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// imageCmd checks an OCI image layout against a pinned linux/arm64 manifest
// digest (check), or stages, checks and loads it into the image store (load).
// The report is printed either way, so a refusal shows its evidence.
func imageCmd(step string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("image "+step, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root (load)")
	hostBin := fs.String("host-bin", "", "signed pomar-host binary (load)")
	layout := fs.String("layout", "", "an OCI image layout directory")
	pin := fs.String("digest", "", "the pinned digest of the image's linux/arm64 manifest")
	name := fs.String("name", "", "the image's reference, as the layout's index.json names it (load)")
	catalogue := fs.String("catalogue", "", "a reviewed local-layout catalogue image (load; derives name and digest)")
	filesManifest := fs.String("files", "", "a sha256sum-format manifest of files the image must hold, byte for byte, in its final filesystem")
	filesRoot := fs.String("files-root", "", "the absolute directory in the image that the manifest's relative paths are under")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var local *smoke.GuestImage
	if *catalogue != "" {
		g, err := smoke.GuestImageByName(*catalogue)
		if err != nil || step != "load" || !g.LocalLayout {
			fmt.Fprintln(stderr, "image load: -catalogue requires a reviewed local-layout image")
			return 2
		}
		if (*name != "" && *name != g.Ref()) || (*pin != "" && *pin != g.Arm64) {
			fmt.Fprintln(stderr, "image load: name or digest differs from the catalogue")
			return 2
		}
		*name, *pin, local = g.Ref(), g.Arm64, &g
	}
	if *layout == "" || *pin == "" || (step == "load" && (*hostBin == "" || *name == "")) {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if !strings.HasPrefix(*pin, "sha256:") || len(*pin) != len("sha256:")+64 {
		fmt.Fprintf(stderr, "image %s: -digest %q is not a sha256 digest\n", step, *pin)
		return 2
	}
	var files map[string]string
	if *filesManifest != "" {
		mf, err := os.Open(*filesManifest)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		files, err = ocilayout.ParseFilesManifest(mf, *filesRoot)
		mf.Close()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if step == "check" {
		r, err := ocilayout.CheckFiles(*layout, *pin, files)
		return imageReport(r, err, stdout, stderr)
	}
	if err := checkImageName(*name); err != nil && local == nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := sign.Check(*hostBin); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := v.CheckHeavy(venue.DefaultMaxFillPercent); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// The copy that is checked is the copy that is loaded, in a directory only
	// Pomar writes, so the layout cannot change between the two.
	id := "image-load-" + strings.TrimPrefix(*pin, "sha256:")[:12]
	rel := filepath.Join("downloads", id)
	if err := v.Intent(venue.KindDownload, venue.ClassAttempt, id, rel, "OCI layout staged to load "+*pin); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer v.Teardown(venue.KindDownload, id)
	staged := filepath.Join(v.Root(), rel)
	r, err := ocilayout.StageFiles(*layout, staged, *pin, files)
	if err != nil {
		v.Failed(venue.KindDownload, id, "staging refused")
		return imageReport(r, err, stdout, stderr)
	}
	if local != nil {
		if err := checkCatalogueLayout(*local, r); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := v.Created(venue.KindDownload, id); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if r.Reference != *name {
		fmt.Fprintf(stderr, "image load: the layout names the image %q, not %q\n", r.Reference, *name)
		imageReport(r, nil, stdout, stderr)
		return 1
	}
	if err := v.EnsureCache(venue.KindImage, "image-store", "store"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out, err := exec.Command(*hostBin, "load-image", "--store", filepath.Join(v.Root(), "store"),
		"--layout", staged, "--manifest", *pin, "--reference", *name).CombinedOutput()
	if err != nil {
		fmt.Fprintf(stderr, "image load: %v: %s", err, out)
		imageReport(r, nil, stdout, stderr)
		return 1
	}
	imageReport(r, nil, stdout, stderr)
	fmt.Fprint(stdout, string(out))
	return 0
}

func checkCatalogueLayout(g smoke.GuestImage, r *ocilayout.Report) error {
	if !g.LocalLayout || r == nil || r.Reference != g.Ref() || r.Root != g.Digest || r.Manifest != g.Arm64 {
		return fmt.Errorf("image load: staged local layout differs from the reviewed catalogue pins")
	}
	return nil
}

// imageReport prints a check's report as JSON, and the refusal if there is
// one; it returns the exit status.
func imageReport(r *ocilayout.Report, err error, stdout, stderr io.Writer) int {
	if r != nil {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(stdout, string(b))
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ocilayout.ErrRefused):
		fmt.Fprintln(stderr, "image: REFUSED:", err)
	default:
		fmt.Fprintln(stderr, "image:", err)
	}
	return 1
}

// checkImageName refuses a name that is not a tagged reference, or that is
// one of Pomar's own pinned images: a load must never retag an image the
// guests or the CI class already use.
func checkImageName(name string) error {
	i := strings.LastIndex(name, ":")
	if strings.Contains(name, "@") || i <= strings.LastIndex(name, "/") || i == len(name)-1 {
		return fmt.Errorf("image load: -name %q must be a tagged reference, repository:tag", name)
	}
	repo := name[:i]
	repos := []string{smoke.InitRepo}
	for _, g := range smoke.GuestImages {
		repos = append(repos, g.Repo)
	}
	for _, own := range repos {
		if repo == own {
			return fmt.Errorf("image load: -name %q is Pomar's own pinned image %s; a load never retags it", name, own)
		}
	}
	return nil
}
