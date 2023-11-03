package config

import (
	"github.com/spf13/viper"
	"reaper/internal/ui"
)

type Config struct {
	Repository []Repository `yaml:"repository"`
	Storage    []Storage    `yaml:"storage"`
}

type Repository struct {
	Name    string   `yaml:"name"`
	URL     string   `yaml:"url"`
	Cron    string   `yaml:"cron"`
	Storage []string `yaml:"storage"`
}

type Storage struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	Path string `yaml:"path"`
}

var Path string

var vp *viper.Viper
var ins *Config

func Init() {
	vp = viper.New()
	vp.SetConfigFile(Path)
	err := vp.ReadInConfig()
	if err != nil {
		ui.ErrorfExit("Error reading config file, %s", err)
	}
	err = vp.Unmarshal(&ins)
	if err != nil {
		ui.ErrorfExit("Error unmarshalling config file, %s", err)
	}
}

func GetIns() *Config {
	return ins
}
