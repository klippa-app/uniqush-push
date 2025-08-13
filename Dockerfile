FROM golang:1.24-alpine AS builder

COPY . /go/src/uniqush-push/
WORKDIR /go/src/uniqush-push

# Build go binary, don't use CGO as that won't run in Alpine.
RUN CGO_ENABLED=0 go build -v

FROM alpine:3.22

RUN apk --no-cache add ca-certificates

COPY --from=builder /go/src/uniqush-push/uniqush-push /usr/bin/uniqush-push

WORKDIR /app

COPY conf/uniqush-push.conf .

RUN mkdir /etc/uniqush/ \
    && cp ./uniqush-push.conf /etc/uniqush/ \
    && sed -i -e 's/localhost/0.0.0.0/' /etc/uniqush/uniqush-push.conf

EXPOSE 9898

CMD ["/usr/bin/uniqush-push"]

HEALTHCHECK CMD curl -f http://localhost:9898/version || exit 1