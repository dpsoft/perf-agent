// Command promo records perf-agent's promo video for X from one real capture.
//
// It renders the capture's profiles with cmd/flamegraph, drives the pages in
// headless Chrome with real mouse and key events, and holds each screenshot
// for a number of frames: a title card, the quick start typed in a terminal,
// one scene per profile type, the GPU telemetry panel, a before/after search,
// go tool pprof's web UI and a closing card. ffmpeg encodes the frames as an
// H.264 MP4 that X accepts. Run it from scripts/promo; see README.md.
package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type config struct {
	cpu, offcpu, gpu, telemetry string
	repo, out, chrome, version  string
	framesDir                   string
}

func main() {
	home, _ := os.UserHomeDir()
	var c config
	flag.StringVar(&c.cpu, "cpu", "", "CPU profile of the capture (pprof, .pb.gz)")
	flag.StringVar(&c.offcpu, "offcpu", "", "off-CPU profile of the same capture")
	flag.StringVar(&c.gpu, "gpu", "", "GPU profile of the same capture")
	flag.StringVar(&c.telemetry, "telemetry", "", "NVML telemetry JSONL of the same capture")
	flag.StringVar(&c.repo, "repo", "../..", "perf-agent checkout whose cmd/flamegraph renders the pages")
	flag.StringVar(&c.out, "o", filepath.Join(home, ".cache/promo/perf-agent-promo.mp4"), "MP4 to write")
	flag.StringVar(&c.chrome, "chrome", "/usr/bin/google-chrome", "Chrome or Chromium binary")
	flag.StringVar(&c.version, "version", "1.5.3", "perf-agent version on the closing card")
	flag.StringVar(&c.framesDir, "frames", "", "keep the PNG frames in this directory (default: a temporary one, removed)")
	flag.Parse()
	for name, v := range map[string]string{"cpu": c.cpu, "offcpu": c.offcpu, "gpu": c.gpu, "telemetry": c.telemetry} {
		if v == "" {
			log.Fatalf("-%s is required", name)
		}
	}
	if err := run(c); err != nil {
		log.Fatal(err)
	}
}

func run(c config) error {
	work, err := os.MkdirTemp("", "perf-agent-promo")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	frames := c.framesDir
	if frames == "" {
		frames = filepath.Join(work, "frames")
	}
	if err := os.MkdirAll(frames, 0o755); err != nil {
		return err
	}
	if old, _ := filepath.Glob(filepath.Join(frames, "f*.png")); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}

	pages, err := renderPages(c, work)
	if err != nil {
		return err
	}
	pprofURL, stopPprof, err := startPprof(c.gpu)
	if err != nil {
		return err
	}
	defer stopPprof()

	rec, closeBrowser, err := newRecorder(c.chrome, frames)
	if err != nil {
		return err
	}
	err = record(rec, c, pages, pprofURL, work)
	closeBrowser()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(c.out), 0o755); err != nil {
		return err
	}
	ffmpeg := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-framerate", fmt.Sprint(fps),
		"-i", filepath.Join(frames, "f%05d.png"), "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-crf", "20", "-preset", "slow", "-movflags", "+faststart", c.out)
	ffmpeg.Stdout, ffmpeg.Stderr = os.Stdout, os.Stderr
	if err := ffmpeg.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	st, err := os.Stat(c.out)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d bytes, %.1f s, %d frames at %d fps\n", c.out, st.Size(), float64(rec.n)/fps, rec.n, fps)
	for _, m := range rec.marks {
		fmt.Printf("  %5.1f s  %s\n", float64(m.frame)/fps, m.name)
	}
	return nil
}

// pages are the rendered flame graph pages, as file:// URLs.
type pages struct {
	cpu, offcpu, gpu, fused string
	workload, device        string
}

// renderPages builds cmd/flamegraph from the checkout and renders one page per
// profile plus the fused CPU + GPU page. Absolute paths under the home
// directory (the module list names every mapped file) are shown as ~/.
func renderPages(c config, work string) (pages, error) {
	bin := filepath.Join(work, "flamegraph")
	build := exec.Command("go", "build", "-o", bin, "./cmd/flamegraph")
	build.Dir, build.Stderr = c.repo, os.Stderr
	if err := build.Run(); err != nil {
		return pages{}, fmt.Errorf("build cmd/flamegraph in %s: %w", c.repo, err)
	}
	device := cmp.Or(deviceName(c.telemetry), "GPU")
	const workload = "PyTorch training step"
	home, _ := os.UserHomeDir()
	render := func(name string, args ...string) (string, error) {
		out := filepath.Join(work, name+".html")
		cmd := exec.Command(bin, append([]string{"-o", out}, args...)...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("render %s: %w", name, err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			return "", err
		}
		b = []byte(strings.ReplaceAll(string(b), home+"/", "~/"))
		if strings.Contains(string(b), home) {
			return "", fmt.Errorf("render %s: %s still appears in the page", name, home)
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return "", err
		}
		return "file://" + out, nil
	}
	var p pages
	var err error
	p.workload, p.device = workload, device
	if p.cpu, err = render("cpu", "-title", workload+": CPU, 99 Hz", c.cpu); err != nil {
		return p, err
	}
	if p.offcpu, err = render("offcpu", "-title", workload+": off-CPU", c.offcpu); err != nil {
		return p, err
	}
	if p.gpu, err = render("gpu", "-title", workload+": GPU kernels ("+device+")", c.gpu); err != nil {
		return p, err
	}
	p.fused, err = render("fused", "-fuse", "-gpu-telemetry", c.telemetry,
		"-title", workload+": CPU + GPU ("+device+")",
		"-in", c.cpu+";;[cpu] perf-agent 99 Hz", "-in", c.gpu+";;[gpu] CUDA kernels")
	return p, err
}

// deviceName reads the GPU model from the telemetry's first record, without
// the vendor's brand prefix ("NVIDIA GeForce RTX 3090" -> "RTX 3090").
func deviceName(telemetry string) string {
	f, err := os.Open(telemetry)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	if !sc.Scan() {
		return ""
	}
	var rec struct {
		Device struct {
			Name string `json:"name"`
		} `json:"device"`
	}
	if json.Unmarshal(sc.Bytes(), &rec) != nil {
		return ""
	}
	name := rec.Device.Name
	for _, prefix := range []string{"NVIDIA GeForce ", "NVIDIA "} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			return rest
		}
	}
	return name
}

// startPprof serves go tool pprof's web UI for a profile on a free local port.
// The pprof binary is run directly, not through `go tool`, so stopping it
// leaves no child behind. The profile is opened by its base name from its own
// directory, so no absolute path shows in the UI.
func startPprof(profile string) (string, func(), error) {
	out, err := exec.Command("go", "tool", "-n", "pprof").Output()
	if err != nil {
		return "", nil, fmt.Errorf("locate pprof: %w", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	addr := l.Addr().String()
	l.Close()
	abs, err := filepath.Abs(profile)
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command(strings.TrimSpace(string(out)), "-http="+addr, "-no_browser", filepath.Base(abs))
	cmd.Dir = filepath.Dir(abs)
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop := func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
	url := "http://" + addr + "/ui/flamegraph"
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return url, stop, nil
			}
		}
	}
	stop()
	return "", nil, fmt.Errorf("pprof did not serve %s", url)
}

// record plays the storyboard, about 43 seconds in all.
func record(r *recorder, c config, p pages, pprofURL, work string) error {
	cards, err := writeCards(work, c.repo, c.version)
	if err != nil {
		return err
	}
	return r.try(func() {
		r.scene("title")
		r.card(cards.title, 3.0)

		r.scene("terminal")
		r.terminal(cards.terminal)

		r.scene("cpu")
		r.cpuScene(p.cpu)

		r.scene("off-cpu")
		r.flame(p.offcpu, "Off-CPU", "where threads block: futex waits, poll and sleep", 2.4)

		r.scene("gpu")
		r.flame(p.gpu, "GPU kernels", "device time, under the Python stack that launched it", 2.4)

		r.scene("fused")
		r.fusedScene(p.fused)

		r.scene("telemetry")
		r.telemetryScene()

		r.scene("before/after")
		r.beforeAfter(p.cpu, p.gpu, "pa_activation")

		r.scene("pprof")
		r.open(pprofURL, "body")
		r.caption("Standard pprof, GPU too", "go tool pprof -http :8080 gpu.pb.gz")
		r.snap(3.0)

		r.scene("closing")
		r.card(cards.closing, 4.5)
	})
}
