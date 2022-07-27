package nktest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// GetImageTags gets the docker registry tags for a image id.
func GetImageTags(ctx context.Context, h Handler, id string) ([]string, error) {
	tok, err := GenDockerToken(ctx, h, id)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", h.DockerRegistryURL()+"/v2/"+id+"/tags/list", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", "Bearer "+tok)
	res, err := h.HttpClient().Do(req)
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

// GenDockerToken generates a docker auth token for the repo id.
func GenDockerToken(ctx context.Context, h Handler, id string) (string, error) {
	q := url.Values{
		"service": []string{h.DockerAuthName()},
		"scope":   []string{h.DockerAuthScope(id)},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", h.DockerTokenURL()+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	res, err := h.HttpClient().Do(req)
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
