FROM        golang:alpine as builder
WORKDIR     $GOPATH/src/
RUN         apk --no-cache add git
#CMD         tail -f /dev/null
COPY        . . 
RUN         export CGO_ENABLED=0
RUN         cd s3-helper \
         && go get \
         && go build

FROM        alpine:latest
WORKDIR     /root/
COPY        --from=builder /go/src/s3-helper/s3-helper .
CMD         ./s3-helper
