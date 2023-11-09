# REAPER

English | [简体中文](README_zh.md)

REpository ArchivER(REAPER) is a tool to archive repositories from any Git servers.

- [Installation](#installation)
- [Usage](#usage)
  - [rip](#rip)
  - [run](#run)
  - [daemon](#daemon)
- [Configuration](#configuration)
- [Storage](#storage)
- [Run as docker container](#run-as-docker-container)
  - [Docker CLI](#docker-cli)
  - [Docker Compose](#docker-compose)

## Installation

```bash
go install github.com/leslieleung/reaper@latest
```

Or get from [Release](https://github.com/LeslieLeung/reaper/releases).

## Usage

You have to create a configuration file to use REAPER.

```yaml
repository:
  - name: reaper
    url: github.com/leslieleung/reaper
    storage:
      - localFile

storage:
  - name: localFile
    type: file
    path: ./repo
```

Then you can run REAPER with the configuration file.

```bash
reaper -c config.yaml
# or simply call reaper if your configuration file is named config.yaml
reaper
```

### rip

`rip` archives a single repository defined in configuration.

```bash
reaper rip reaper
```

### run

`run` archives all repositories defined in configuration.

```bash
reaper run
```

Combined with cron, you can archive repositories periodically.

### daemon

`daemon` runs REAPER as a daemon. It will archive all repositories defined in configuration periodically.

```bash
reaper daemon
# You might want to run it with something like nohup
nohup reaper daemon &
```

## Configuration

For configuration, you can checkout this [example](config/example.config.yaml).

## Storage

REAPER supports multiple storage types.

- [x] File
- [x] AWS S3

## Run as docker container

### Docker CLI

One-off run. 
- Change `${pwd}/config/example.config.yaml` to your config file path.
- Customize `${pwd}/repo:/repo` to be your desired storage path. The in-container path needs to be the same as the path in config file.

```bash
docker run --rm \
    -v ${pwd}/config/example.config.yaml:/config.yaml \
    -v ${pwd}/repo:/repo \
    leslieleung/reaper:latest \
    run
```

### Docker Compose

For example compose file, see [docker-compose.yml](docker-compose.yml).

```bash
docker compose up -d
```

## FAQ

See [FAQ](https://github.com/LeslieLeung/reaper/wiki/FAQ).

## Stargazers over time

[![Stargazers over time](https://starchart.cc/LeslieLeung/reaper.svg)](https://starchart.cc/LeslieLeung/reaper)

