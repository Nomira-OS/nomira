package frontend

import (
	"encoding/json"
	"fmt"
)

const descriptionLabel = "org.opencontainers.image.description"

// imageConfig derives the exported image's config from the base image's:
// it keeps everything (entrypoint, env, the containers.bootc label …), adds
// the recipe's labels and description, and drops rootfs and history, which
// describe the base's layers and are rebuilt by the exporter.
func imageConfig(base []byte, description string, labels map[string]string) ([]byte, error) {
	var img map[string]any
	if err := json.Unmarshal(base, &img); err != nil {
		return nil, fmt.Errorf("parse base image config: %w", err)
	}
	delete(img, "rootfs")
	delete(img, "history")

	if description != "" || len(labels) > 0 {
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
		if description != "" {
			merged[descriptionLabel] = description
		}
		for k, v := range labels {
			merged[k] = v
		}
	}
	return json.Marshal(img)
}
