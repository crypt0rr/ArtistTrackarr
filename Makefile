.PHONY: test build run

test:
	docker build --target test .

build:
	docker build -t artist-trackarr:local .

run:
	docker compose up --build
