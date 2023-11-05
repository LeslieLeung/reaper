FROM alpine:3.18.4 as alpine

RUN apk add --no-cache \
    ca-certificates \
    tzdata

# initial default temp dir
RUN mkdir /tmp/reaper

# minimal image
FROM scratch
COPY --from=alpine \
    /etc/ssl/certs/ca-certificates.crt \
    /etc/ssl/certs/ca-certificates.crt
COPY --from=alpine \
    /usr/share/zoneinfo \
    /usr/share/zoneinfo
COPY --from=alpine \
    /tmp/reaper \
    /tmp/reaper

COPY reaper /usr/local/bin/reaper

WORKDIR /reaper

ENTRYPOINT ["reaper","-c","/reaper/config.yaml"]
CMD ["run"]