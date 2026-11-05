FROM golang:1-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY . .
RUN go mod download
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -a -installsuffix cgo -o ./out/turbogmailbridge .
# Bridge state directory; a named volume mounted here inherits the owner.
RUN mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /src/out/turbogmailbridge /main
COPY --from=build --chown=10001:10001 /out/data /data
USER 10001
ENTRYPOINT ["/main"]
CMD ["/turbogmailbridge.conf"]
