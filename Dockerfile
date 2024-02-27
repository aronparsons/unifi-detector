# build
FROM registry.fedoraproject.org/fedora:latest AS build
WORKDIR /app
RUN dnf -y install go make

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make build


# runtime
FROM registry.fedoraproject.org/fedora:latest
COPY --from=build /app/bin/unifi-detector /unifi-detector
CMD ["/unifi-detector"]
