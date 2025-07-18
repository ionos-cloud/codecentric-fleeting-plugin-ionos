FROM golang as build

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download -x

COPY . .
RUN go build -o fleeting-plugin-ionos cmd/fleeting-plugin-ionos/main.go
FROM gitlab/gitlab-runner

RUN apt-get update && apt-get install -y iputils-ping net-tools iproute2 curl && apt-get install -y vim

COPY --from=build /app/fleeting-plugin-ionos /usr/local/bin/fleeting-plugin-ionos
COPY ./test/config.toml /etc/gitlab-runner/config.toml
COPY ./test/key /etc/gitlab-runner/keys/key
