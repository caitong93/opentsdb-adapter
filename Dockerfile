FROM alpine:3.6

# Use aliyun source
RUN echo "http://mirrors.aliyun.com/alpine/v3.6/main" > /etc/apk/repositories
RUN echo "http://mirrors.aliyun.com/alpine/v3.6/community" >> /etc/apk/repositories

RUN apk update && apk add -t .base curl bash tzdata ca-certificates \
  && cp -r -f /usr/share/zoneinfo/Asia/Shanghai /etc/localtime

COPY adapter /adapter
ENTRYPOINT ["/adapter"]

