.PHONY:dry-build
dry-build:
	go build -o reaper main.go
	rm -r reaper