# SPDX-License-Identifier: MIT

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kiosk ./cmd/kiosk
# distroless has no shell to mkdir with: /data is made here and copied owned.
RUN mkdir /out/data

# uid 65532 (nonroot) on purpose: it is zlatan's uid, so the per-user download
# directories the broker creates in zlatan's staging belong to zlatan.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kiosk /usr/local/bin/kiosk
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/kiosk"]
