.PHONY: test control-plane web-build compose-up

test:
	go test ./...

control-plane:
	go run ./cmd/control-plane -addr :8080

web-build:
	cd web-console && npm install && npm run build

compose-up:
	cd deploy/compose && docker compose --env-file .env up -d
