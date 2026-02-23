package plan

import (
	"os"

	yamlv3 "gopkg.in/yaml.v3"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func readFileIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

func unmarshalYAML(data []byte, v any) error {
	return yamlv3.Unmarshal(data, v)
}

func writeYAMLAtomic(path string, data any) error {
	return yamlutil.AtomicWrite(path, data)
}
