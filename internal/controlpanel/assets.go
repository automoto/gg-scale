package controlpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const controlPanelAssetMountPath = "/v1/control-panel/assets/"

var controlPanelAssetVersions = computeControlPanelAssetVersions()

func computeControlPanelAssetVersions() map[string]string {
	out := map[string]string{}
	entries, err := staticFS.ReadDir("static")
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data, rerr := staticFS.ReadFile("static/" + name)
		if rerr != nil {
			continue
		}
		sum := sha256.Sum256(data)
		out[name] = hex.EncodeToString(sum[:4])
	}
	return out
}

func controlPanelAssetURL(name string) string {
	u := controlPanelAssetMountPath + name
	if strings.Contains(name, "..") {
		return u
	}
	if v, ok := controlPanelAssetVersions[name]; ok {
		u += "?v=" + v
	}
	return u
}
