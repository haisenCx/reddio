package config

type Config struct {
	IsParallel  bool
	Concurrency int
}

func defaultConfig() *Config {
	return &Config{
		IsParallel:  true,
		Concurrency: 16,
	}
}

var GlobalConfig *Config

func init() {
	GlobalConfig = defaultConfig()
}

func GetGlobalConfig() *Config {
	return GlobalConfig
}
