FROM alpine:3.18.4 as alpine

RUN apk add --no-cache \
    ca-certificates \
    tzdata

# minimal image
FROM scratch
COPY --from=alpine \
    /etc/ssl/certs/ca-certificates.crt \
    /etc/ssl/certs/ca-certificates.crt
COPY --from=alpine \
    /usr/share/zoneinfo \
    /usr/share/zoneinfo

COPY reaper /

ENTRYPOINT ["/reaper"]
CMD ["run"]