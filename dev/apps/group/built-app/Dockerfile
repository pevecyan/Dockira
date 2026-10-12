ARG ALPINE_VERSION=3.20
FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache busybox-extras
CMD ["sh", "-c", "mkdir -p /www && echo built-app > /www/index.html && httpd -f -p 8000 -h /www"]
