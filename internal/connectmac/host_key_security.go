package connectmac

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type hostKeyChallenge struct {
	Profile, Host, Digest string
	Port                  int
	ExpiresAt             time.Time
}

type hostKeyChallengeRegistry struct {
	mu      sync.Mutex
	entries map[string]hostKeyChallenge
	max     int
	ttl     time.Duration
	now     func() time.Time
}

func newHostKeyChallengeRegistry(max int, ttl time.Duration) *hostKeyChallengeRegistry {
	return &hostKeyChallengeRegistry{entries: map[string]hostKeyChallenge{}, max: max, ttl: ttl, now: time.Now}
}

func (r *hostKeyChallengeRegistry) Issue(profile, host string, port int, fingerprints []string) (string, error) {
	if r == nil {
		return "", errors.New("host key challenge registry is not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.prune(now)
	if len(r.entries) >= r.max {
		return "", errors.New("host key challenge registry is full")
	}
	for attempts := 0; attempts < 4; attempts++ {
		token, err := randomToken(32)
		if err != nil {
			return "", err
		}
		if _, exists := r.entries[token]; exists {
			continue
		}
		r.entries[token] = hostKeyChallenge{Profile: profile, Host: normalizeHostKeyHost(host), Port: port, Digest: fingerprintSetDigest(fingerprints), ExpiresAt: now.Add(r.ttl)}
		return token, nil
	}
	return "", errors.New("generate unique host key challenge")
}

func (r *hostKeyChallengeRegistry) Consume(token, profile, host string, port int, fingerprints []string) error {
	if r == nil {
		return errors.New("host key challenge registry is not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.prune(now)
	entry, ok := r.entries[token]
	delete(r.entries, token)
	if !ok || !entry.ExpiresAt.After(now) {
		return errors.New("host key challenge is invalid, expired, or already used")
	}
	if entry.Profile != profile || entry.Host != normalizeHostKeyHost(host) || entry.Port != port || entry.Digest != fingerprintSetDigest(fingerprints) {
		return errors.New("host key challenge does not match the confirmed host keys")
	}
	return nil
}

func (r *hostKeyChallengeRegistry) prune(now time.Time) {
	for token, entry := range r.entries {
		if !entry.ExpiresAt.After(now) {
			delete(r.entries, token)
		}
	}
}

type hostKeyBlockedEventCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	max     int
	ttl     time.Duration
	now     func() time.Time
}

func newHostKeyBlockedEventCache(max int, ttl time.Duration) *hostKeyBlockedEventCache {
	return &hostKeyBlockedEventCache{entries: map[string]time.Time{}, max: max, ttl: ttl, now: time.Now}
}
func (c *hostKeyBlockedEventCache) First(key string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, expires := range c.entries {
		if !expires.After(now) {
			delete(c.entries, k)
		}
	}
	if expires, ok := c.entries[key]; ok && expires.After(now) {
		return false
	}
	if len(c.entries) >= c.max {
		var oldest string
		var at time.Time
		for k, expires := range c.entries {
			if oldest == "" || expires.Before(at) {
				oldest, at = k, expires
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = now.Add(c.ttl)
	return true
}

var knownHostsPathLocks sync.Map

func normalizedKnownHostsPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		info, statErr := os.Stat(resolved)
		if statErr != nil {
			return "", fmt.Errorf("stat resolved known_hosts path: %w", statErr)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("resolved known_hosts path is not a regular file")
		}
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve known_hosts path: %w", err)
	}
	if info, lstatErr := os.Lstat(abs); lstatErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("known_hosts path is a broken symbolic link")
		}
		return "", fmt.Errorf("inspect known_hosts path after resolution failure: %w", err)
	} else if !os.IsNotExist(lstatErr) {
		return "", fmt.Errorf("inspect known_hosts path: %w", lstatErr)
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve known_hosts directory: %w", err)
	}
	return filepath.Join(resolvedDir, filepath.Base(abs)), nil
}

func normalizedKnownHostsPathLock(path string) *sync.Mutex {
	value, _ := knownHostsPathLocks.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}
func normalizeHostKeyHost(host string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
}

func normalizeFingerprintSet(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
func fingerprintSetDigest(values []string) string {
	sum := sha256.Sum256([]byte(strings.Join(normalizeFingerprintSet(values), "\n")))
	return hex.EncodeToString(sum[:])
}

func replaceKnownHostKeys(content []byte, host, scanned string) ([]byte, error) {
	if len(hostKeyPairs(scanned)) == 0 {
		return nil, errors.New("validated scanned host keys are required")
	}
	var lines []string
	for _, line := range strings.Split(string(content), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		hostField := 0
		if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
			hostField = 1
		}
		if len(fields) > hostField && knownHostFieldMatches(fields[hostField], host) {
			continue
		}
		lines = append(lines, line)
	}
	lines = append(lines, strings.Split(strings.TrimSpace(scanned), "\n")...)
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func knownHostFieldMatches(field, host string) bool {
	for _, candidate := range strings.Split(field, ",") {
		if knownHostTokenMatches(candidate, host) {
			return true
		}
	}
	return false
}
func knownHostTokenMatches(token, host string) bool {
	host = normalizeHostKeyHost(host)
	plain := normalizeHostKeyHost(token)
	if plain == host || strings.EqualFold(strings.TrimSpace(token), "["+host+"]:22") {
		return true
	}
	parts := strings.Split(token, "|")
	if len(parts) != 4 || parts[1] != "1" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	for _, candidate := range []string{host, "[" + host + "]:22"} {
		mac := hmac.New(sha1.New, salt)
		_, _ = mac.Write([]byte(candidate))
		if hmac.Equal(mac.Sum(nil), want) {
			return true
		}
	}
	return false
}

func writeFileAtomically(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".known_hosts-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(name)
		}
	}()
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	cleanup = false
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func knownHostsMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0600
}
func readKnownHosts(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}
func hostKeyChallengePort() int      { return 22 }
func challengeError(err error) error { return fmt.Errorf("host key confirmation failed: %w", err) }
