package linuxdrm

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

type cgroupPathMetadata struct {
	PodUID           string
	ContainerID      string
	ContainerRuntime string
}

type cgroupPathLookup func(pid uint32) (string, bool)

type cgroupPathCache struct {
	mu      sync.Mutex
	lookup  cgroupPathLookup
	entries map[uint32]cgroupPathResult
}

type cgroupPathResult struct {
	path string
	ok   bool
}

func lookupCgroupPath(pid uint32) (string, bool) {
	return lookupCgroupPathFrom(procRoot, pid)
}

func newCgroupPathCache(lookup cgroupPathLookup) *cgroupPathCache {
	return &cgroupPathCache{
		lookup:  lookup,
		entries: make(map[uint32]cgroupPathResult),
	}
}

func (c *cgroupPathCache) Lookup(pid uint32) (string, bool) {
	c.mu.Lock()
	if result, ok := c.entries[pid]; ok {
		c.mu.Unlock()
		return result.path, result.ok
	}
	c.mu.Unlock()

	path, ok := c.lookup(pid)

	c.mu.Lock()
	c.entries[pid] = cgroupPathResult{path: path, ok: ok}
	c.mu.Unlock()
	return path, ok
}

func lookupCgroupPathFrom(root string, pid uint32) (string, bool) {
	path := filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "cgroup")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return parseCgroupPath(string(data))
}

func parseCgroupPath(raw string) (string, bool) {
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

func parseCgroupPathMetadata(path string) cgroupPathMetadata {
	var meta cgroupPathMetadata
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
