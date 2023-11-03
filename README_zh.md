# REAPER

[English](README.md) | 简体中文

REpository ArchivER（REAPER）是一个用于从任何Git服务器归档 Git 仓库的工具。

## 安装

```bash
go install github.com/leslieleung/reaper@latest
```

或从 [Release](https://github.com/LeslieLeung/reaper/releases) 获取。 

## 使用方法

你需要创建一个配置文件来使用REAPER。

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

然后，你可以使用配置文件运行REAPER。

```bash
reaper -c config.yaml
# 或者如果你的配置文件名为config.yaml，只需调用reaper
reaper
```

### rip

`rip`命令会归档在配置中定义的单个 Git 仓库。

```bash
reaper rip reaper
```

### run

`run`命令会归档在配置中定义的所有 Git 仓库。

```bash
reaper run
```

结合cron，你可以定期归档 Git 仓库。

## 配置

有关配置，你可以查看此[示例](config/example.config.yaml)。

### 存储

REAPER支持多种存储类型。

- [x] 文件
- [ ] AWS S3