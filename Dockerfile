FROM golang as build

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download -x

COPY . .
RUN go build -o fleeting-plugin-ionos cmd/fleeting-plugin-ionos/main.go
FROM gitlab/gitlab-runner

COPY --from=build /app/fleeting-plugin-ionos /usr/local/bin/fleeting-plugin-ionos
COPY ./test/config.toml /etc/gitlab-runner/config.toml
COPY ./test/key /etc/gitlab-runner/keys/key
