.PHONY: test build run

test:
	docker build --target test .

build:
	docker build -t artist-release-tracker:local .

run:
	docker compose up --build
