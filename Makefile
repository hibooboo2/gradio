.PHONY: run build play docker-db

COCKROACH_IMAGE ?= cockroachdb/cockroach:latest
COCKROACH_CONTAINER ?= gradio-cockroach
COCKROACH_PORT ?= 26257
COCKROACH_VOLUME ?= gradio-cockroach-data

docker-db:
	@if [ -z "$$(docker ps -aq -f name=^/$(COCKROACH_CONTAINER)$$)" ]; then \
		echo "Starting CockroachDB container '$(COCKROACH_CONTAINER)'..."; \
		docker run -d \
			--name $(COCKROACH_CONTAINER) \
			-p $(COCKROACH_PORT):26257 \
			-v $(COCKROACH_VOLUME):/cockroach/cockroach-data \
			$(COCKROACH_IMAGE) \
			start-single-node \
			--insecure \
			--advertise-addr=localhost; \
	elif [ -z "$$(docker ps -q -f name=^/$(COCKROACH_CONTAINER)$$)" ]; then \
		echo "Starting existing CockroachDB container '$(COCKROACH_CONTAINER)'..."; \
		docker start $(COCKROACH_CONTAINER); \
	fi
	@echo "Waiting for CockroachDB to be ready..."
	@for i in $$(seq 1 60); do \
		if docker exec $(COCKROACH_CONTAINER) cockroach sql --insecure -e "SELECT 1" >/dev/null 2>&1; then \
			echo "CockroachDB is ready on localhost:$(COCKROACH_PORT)."; \
			break; \
		fi; \
		sleep 1; \
	done

run: docker-db
	go build -o gradio
	./gradio -http 8000 -watch

build:
	GOOS=windows go build -o gradio

play: docker-db
	go build -o gradio
	./gradio -play
