package openstack

import (
	"embed"
	"fmt"
	"strings"

	"github.com/cloudfoundry/bosh-bootloader/storage"
)

const templatesPath = "./templates"

type templates struct {
	providerVars     string
	provider         string
	resourcesOutputs string
	resourcesVars    string
	resources        string
}

type TemplateGenerator struct {
	EmbedData embed.FS
	Path      string
}

//go:embed templates
var contents embed.FS

func NewTemplateGenerator() TemplateGenerator {
	return TemplateGenerator{
		EmbedData: contents,
		Path:      "templates",
	}
}

func (t TemplateGenerator) Generate(state storage.State) string {
	tmpls := t.readTemplates()
	template := strings.Join([]string{tmpls.providerVars, tmpls.provider, tmpls.resourcesOutputs, tmpls.resourcesVars, tmpls.resources}, "\n")
	return template
}

func (t TemplateGenerator) readTemplates() templates {
	listings := map[string]string{
		"provider-vars.tf":     "",
		"provider.tf":          "",
		"resources-outputs.tf": "",
		"resources-vars.tf":    "",
		"resources.tf":         "",
	}

	var errors []error
	for item := range listings {
		content, err := t.EmbedData.ReadDir(t.Path)
		for _, embedDataEntry := range content {
			if strings.Contains(embedDataEntry.Name(), item) {
				out, err := t.EmbedData.ReadFile(fmt.Sprintf("%s/%s", t.Path, embedDataEntry.Name()))
				if err != nil {
					errors = append(errors, err)
					break
				}
				listings[item] = string(out)
				break
			}
		}
		if err != nil {
			errors = append(errors, err)
			continue
		}
	}

	if errors != nil {
		panic(errors)
	}

	return templates{
		providerVars:     listings["provider-vars.tf"],
		provider:         listings["provider.tf"],
		resourcesOutputs: listings["resources-outputs.tf"],
		resourcesVars:    listings["resources-vars.tf"],
		resources:        listings["resources.tf"],
	}
}
