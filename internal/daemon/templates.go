package daemon

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

var templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var templateDigest = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9./_:-]*@)?sha256:[0-9a-f]{64}$`)

func validateTemplates(templates map[string]string) error {
	for name, image := range templates {
		if !templateName.MatchString(name) || !templateDigest.MatchString(image) {
			return fmt.Errorf("template %q requires a valid name and immutable sha256 image", name)
		}
	}
	return nil
}

func (p DeploymentPolicy) currentTemplates() (map[string]string, error) {
	if p.catalogPath == "" {
		return p.Templates, nil
	}
	var catalog struct {
		Templates map[string]string `json:"templates"`
	}
	data, err := os.ReadFile(p.catalogPath)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	return catalog.Templates, validateTemplates(catalog.Templates)
}

func (p DeploymentPolicy) ResolveImage(image string) (string, error) {
	name, alias := strings.CutPrefix(image, "template:")
	if !alias {
		return image, nil
	}
	catalog, err := p.currentTemplates()
	if err != nil {
		return "", errorf(CodeUnavailable, "template catalog unavailable")
	}
	digest, ok := catalog[name]
	if !ok {
		return "", errorf(CodeInvalidRequest, "unknown template %q", name)
	}
	return digest, nil
}

func (b *PolicyBackend) ResolveImage(image string) (string, error) {
	return b.policy.ResolveImage(image)
}

func (p DeploymentPolicy) allowsImage(image string) bool {
	if slices.Contains(p.Images, image) {
		return true
	}
	catalog, err := p.currentTemplates()
	if err != nil {
		return false
	}
	for _, digest := range catalog {
		if digest == image {
			return true
		}
	}
	return false
}
