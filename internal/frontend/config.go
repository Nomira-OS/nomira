package frontend

import (
	"encoding/json"
	"fmt"
	"time"
)

// imageConfig derives the exported image's config from the base image's:
// it keeps everything (entrypoint, env, the containers.bootc label …), adds
// the labels, and drops rootfs and history, which
// describe the base's layers and are rebuilt by the exporter.
//
// The creation time is the source date epoch; without one (nil) the
// exporter sets the time of the build.
func imageConfig(base []byte, labels map[string]string, epoch *int64) ([]byte, error) {
	var img map[string]any
	if err := json.Unmarshal(base, &img); err != nil {
		return nil, fmt.Errorf("parse base image config: %w", err)
	}
	delete(img, "rootfs")
	delete(img, "history")
	if epoch != nil {
		img["created"] = time.Unix(*epoch, 0).UTC().Format(time.RFC3339)
	} else {
		delete(img, "created")
	}

	if len(labels) > 0 {
		cfg, _ := img["config"].(map[string]any)
		if cfg == nil {
			cfg = map[string]any{}
			img["config"] = cfg
		}
		merged, _ := cfg["Labels"].(map[string]any)
		if merged == nil {
			merged = map[string]any{}
			cfg["Labels"] = merged
		}
		for k, v := range labels {
			merged[k] = v
		}
	}
	return json.Marshal(img)
}

// sourceDateEpoch returns settings.source_date_epoch, or else the creation
// time of the base image, or else 0. With the base image's time a recipe
// that sets nothing still builds the same image every time from the same
// base, and the image is no newer than what it is built from.
func sourceDateEpoch(set *int64, base []byte) int64 {
	if set != nil {
		return *set
	}
	var img struct {
		Created *time.Time `json:"created"`
	}
	if json.Unmarshal(base, &img) == nil && img.Created != nil && img.Created.Unix() > 0 {
		return img.Created.Unix()
	}
	return 0
}
