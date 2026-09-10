FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /loopmath-agent ./cmd/loopmath-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /loopmath-agent /loopmath-agent
EXPOSE 8787 8788
ENV LOOPMATH_ADMIN_ADDR=:8788 LOOPMATH_FINDINGS_FILE=
ENTRYPOINT ["/loopmath-agent"]
