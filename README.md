# REAPER

English | [简体中文](README_zh.md)

REpository ArchivER(REAPER) is a tool to archive repositories from any Git servers.

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
    path: /tmp/reaper
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


## Configuration

For configuration, you can checkout this [example](config/example.config.yaml).

### Storage

REAPER supports multiple storage types.

- [x] File
- [ ] AWS S3