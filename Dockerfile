FROM alpine:3.18.4
WORKDIR /app
COPY reaper /app/reaper

CMD ["/app/reaper"]