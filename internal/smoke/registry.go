package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// registryHost maps a reference's registry to its API host.
func registryHost(repo string) (host, path string, err error) {
	i := strings.IndexByte(repo, '/')
	if i < 0 {
		return "", "", fmt.Errorf("smoke: reference %q has no registry", repo)
	}
	host, path = repo[:i], repo[i+1:]
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	return host, path, nil
}

// TagDigest asks the registry which digest a tag points at now, using an
// anonymous bearer token when the registry demands one.
func TagDigest(ctx context.Context, client *http.Client, repo, tag string) (string, error) {
	host, path, err := registryHost(repo)
	if err != nil {
		return "", err
	}
	u := "https://" + host + "/v2/" + path + "/manifests/" + tag
	resp, err := head(ctx, client, u, "")
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		tok, err := anonymousToken(ctx, client, resp.Header.Get("Www-Authenticate"))
		if err != nil {
			return "", err
		}
		if resp, err = head(ctx, client, u, tok); err != nil {
			return "", err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("smoke: %s:%s: registry answered %s", repo, tag, resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("smoke: %s:%s: no Docker-Content-Digest", repo, tag)
	}
	return d, nil
}

// CheckPinned fails unless the tag still points at the pinned digest.
func CheckPinned(ctx context.Context, client *http.Client, repo, tag, pinned string) error {
	got, err := TagDigest(ctx, client, repo, tag)
	if err != nil {
		return err
	}
	if got != pinned {
		return fmt.Errorf("smoke: %s:%s now points at %s, pinned %s", repo, tag, got, pinned)
	}
	return nil
}

func head(ctx context.Context, client *http.Client, u, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

// parseChallenge reads a `Bearer realm="…",service="…",scope="…"` header.
func parseChallenge(h string) (map[string]string, error) {
	rest, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return nil, fmt.Errorf("smoke: unsupported auth challenge %q", h)
	}
	out := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(v, `"`)
	}
	if out["realm"] == "" {
		return nil, fmt.Errorf("smoke: auth challenge without realm: %q", h)
	}
	return out, nil
}

func anonymousToken(ctx context.Context, client *http.Client, challenge string) (string, error) {
	c, err := parseChallenge(challenge)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	for _, k := range []string{"service", "scope"} {
		if c[k] != "" {
			q.Set(k, c[k])
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("smoke: token endpoint answered %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}
