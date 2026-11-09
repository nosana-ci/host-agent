package podman

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DiskUsage is /system/df: what each image, container and volume takes.
type DiskUsage struct {
	Images []struct {
		Repository string `json:"Repository"`
		Tag        string `json:"Tag"`
		ImageID    string `json:"ImageID"`
		UniqueSize int64  `json:"UniqueSize"`
		Containers int    `json:"Containers"`
	} `json:"Images"`
	Containers []struct {
		ContainerID string `json:"ContainerID"`
		Names       string `json:"Names"`
		Image       string `json:"Image"`
		Status      string `json:"Status"`
		RWSize      int64  `json:"RWSize"`
	} `json:"Containers"`
	Volumes []struct {
		VolumeName string `json:"VolumeName"`
		Size       int64  `json:"Size"`
	} `json:"Volumes"`
}

func (c *Client) DiskUsage(ctx context.Context) (*DiskUsage, error) {
	var df DiskUsage
	return &df, c.call(ctx, http.MethodGet, "/system/df", nil, nil, &df)
}

// Image is one entry of /images/json.
type Image struct {
	ID         string   `json:"Id"`
	RepoTags   []string `json:"RepoTags"`
	Size       int64    `json:"Size"`
	Containers int      `json:"Containers"`
}

// Ref is the image's first tag, or its short id when it has none.
func (i Image) Ref() string {
	if len(i.RepoTags) > 0 {
		return strings.TrimPrefix(i.RepoTags[0], "docker.io/")
	}
	return ShortID(i.ID)
}

func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var list []Image
	return list, c.call(ctx, http.MethodGet, "/images/json", nil, nil, &list)
}

// RemoveImage deletes an image nothing uses; one a container uses is refused by podman.
func (c *Client) RemoveImage(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodDelete, "/images/"+url.PathEscape(id), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ContainerSummary is one entry of /containers/json?all=true.
type ContainerSummary struct {
	ID        string            `json:"Id"`
	Names     []string          `json:"Names"`
	Image     string            `json:"Image"`
	State     string            `json:"State"`
	StartedAt int64             `json:"StartedAt"` // unix seconds
	Labels    map[string]string `json:"Labels"`
}

// Name is the container's first name.
func (c ContainerSummary) Name() string {
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return ShortID(c.ID)
}

func (c *Client) Containers(ctx context.Context) ([]ContainerSummary, error) {
	var list []ContainerSummary
	return list, c.call(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"true"}}, nil, &list)
}

// Volume is one entry of /volumes/json.
type Volume struct {
	Name string `json:"Name"`
}

func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	var list []Volume
	return list, c.call(ctx, http.MethodGet, "/volumes/json", nil, nil, &list)
}

// RemoveVolume deletes a volume, also one a stopped container still names.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), url.Values{"force": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ShortID is the 12-character form of an image or container id.
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Started is when the container started, zero when it has not.
func (c ContainerSummary) Started() time.Time {
	if c.StartedAt == 0 {
		return time.Time{}
	}
	return time.Unix(c.StartedAt, 0).UTC()
}
