package config

import (
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/spf13/viper"
)

type Config struct {
	Repository []Repository   `yaml:"repository"`
	Storage    []MultiStorage `yaml:"storage"`
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

type MultiStorage struct {
	Storage         `mapstructure:",squash"`
	Endpoint        string `yaml:"endpoint"`
	Bucket          string `yaml:"bucket"`
	Region          string `yaml:"region"`
	AccessKeyID     string `yaml:"accessKeyID"`
	SecretAccessKey string `yaml:"secretAccessKey"`
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
