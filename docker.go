package nktest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DockerImageTags gets the docker registry tags for a image id.
func DockerImageTags(ctx context.Context, id string) ([]string, error) {
	tok, err := DockerToken(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.HasPrefix(id, "localhost/"):
		return nil, nil
	case !strings.HasPrefix(id, "docker.io/"):
		return nil, fmt.Errorf("%s is not fully qualified", id)
	case strings.HasPrefix(id, "docker.io/library/"):
		id = strings.TrimPrefix(id, "docker.io/library/")
	default:
		id = strings.TrimPrefix(id, "docker.io/")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", DockerRegistryURL(ctx)+"/v2/"+id+"/tags/list", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", "Bearer "+tok)
	res, err := HttpClient(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d != 200", res.StatusCode)
	}
	var v struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v.Tags, nil
}

// DockerToken generates a docker auth token for the repo id.
func DockerToken(ctx context.Context, id string) (string, error) {
	switch {
	case strings.HasPrefix(id, "localhost/"):
		return "", nil
	case !strings.HasPrefix(id, "docker.io/"):
		return "", fmt.Errorf("%s is not fully qualified", id)
	case strings.HasPrefix(id, "docker.io/library/"):
		id = strings.TrimPrefix(id, "docker.io/library/")
	default:
		id = strings.TrimPrefix(id, "docker.io/")
	}
	q := url.Values{
		"service": []string{DockerAuthName(ctx)},
		"scope":   []string{DockerAuthScope(ctx, id)},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", DockerTokenURL(ctx)+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	res, err := HttpClient(ctx).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var v struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		IssuedAt    string `json:"issued_at"`
	}
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d != 200", res.StatusCode)
	}
	if v.Token == "" {
		return "", fmt.Errorf("empty token for %s", id)
	}
	return v.Token, nil
}
