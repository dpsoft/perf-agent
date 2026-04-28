package cgroupmeta

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const procRoot = "/proc"

var (
	podUIDRe         = regexp.MustCompile(`(?:^|-)pod([0-9a-fA-F][0-9a-fA-F_-]{7,})$`)
	containerdScope  = regexp.MustCompile(`^cri-containerd-([0-9a-fA-F]{16,})\.scope$`)
	crioScope        = regexp.MustCompile(`^crio-([0-9a-fA-F]{16,})\.scope$`)
	dockerScope      = regexp.MustCompile(`^docker-([0-9a-fA-F]{16,})\.scope$`)
	hexContainerIDRe = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
)

type Metadata struct {
	PodUID           string
	ContainerID      string
	ContainerRuntime string
}

type PathLookup func(pid uint32) (string, bool)

type PathCache struct {
	mu      sync.Mutex
	lookup  PathLookup
	entries map[uint32]pathResult
}

type pathResult struct {
	path string
	ok   bool
}

func LookupPath(pid uint32) (string, bool) {
	return LookupPathFrom(procRoot, pid)
}

func LookupPathFrom(root string, pid uint32) (string, bool) {
	path := filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "cgroup")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return parsePath(string(data))
}

func NewPathCache(lookup PathLookup) *PathCache {
	return &PathCache{
		lookup:  lookup,
		entries: make(map[uint32]pathResult),
	}
}

func (c *PathCache) Lookup(pid uint32) (string, bool) {
	c.mu.Lock()
	if result, ok := c.entries[pid]; ok {
		c.mu.Unlock()
		return result.path, result.ok
	}
	c.mu.Unlock()

	path, ok := c.lookup(pid)

	c.mu.Lock()
	c.entries[pid] = pathResult{path: path, ok: ok}
	c.mu.Unlock()
	return path, ok
}

func MetadataFromPath(path string) Metadata {
	var meta Metadata
	for _, segment := range strings.Split(path, "/") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}

		trimmed := strings.TrimSuffix(segment, ".slice")
		if meta.PodUID == "" {
			if match := podUIDRe.FindStringSubmatch(trimmed); match != nil {
				meta.PodUID = strings.ReplaceAll(match[1], "_", "-")
				continue
			}
		}
		if meta.ContainerID != "" {
			continue
		}
		if match := containerdScope.FindStringSubmatch(segment); match != nil {
			meta.ContainerRuntime = "containerd"
			meta.ContainerID = strings.ToLower(match[1])
			continue
		}
		if match := crioScope.FindStringSubmatch(segment); match != nil {
			meta.ContainerRuntime = "crio"
			meta.ContainerID = strings.ToLower(match[1])
			continue
		}
		if match := dockerScope.FindStringSubmatch(segment); match != nil {
			meta.ContainerRuntime = "docker"
			meta.ContainerID = strings.ToLower(match[1])
			continue
		}
		if hexContainerIDRe.MatchString(segment) {
			meta.ContainerID = strings.ToLower(segment)
		}
	}
	return meta
}

func parsePath(raw string) (string, bool) {
	var fallback string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := strings.TrimSpace(parts[2])
		if path == "" {
			continue
		}
		if parts[1] == "" {
			return path, true
		}
		if fallback == "" {
			fallback = path
		}
	}
	if fallback == "" {
		return "", false
	}
	return fallback, true
}
