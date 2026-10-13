package typedef

import "fmt"

type Storage struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	Path string `yaml:"path"`
}

type MultiStorage struct {
	Storage `yaml:",inline" mapstructure:",squash"`
}

// ValidateType rejects unsupported backends before a configuration is applied.
func (s Storage) ValidateType() error {
	if s.Type != "file" {
		return fmt.Errorf("storage %q has unsupported type %q: only 'file' storage is supported", s.Name, s.Type)
	}
	return nil
}
