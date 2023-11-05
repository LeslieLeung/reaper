FROM alpine:3.18.4
COPY reaper /usr/local/bin/reaper
WORKDIR /reaper
CMD ["reaper"]