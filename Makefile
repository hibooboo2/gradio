.PHONY: run build play docker-db clean_data test

COCKROACH_IMAGE ?= cockroachdb/cockroach:latest
COCKROACH_CONTAINER ?= gradio-cockroach
COCKROACH_PORT ?= 59786
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
	DATABASE_URL=postgres://root@localhost:$(COCKROACH_PORT)/defaultdb?sslmode=disable ./gradio -http 8000 -watch -record

build:
	GOOS=windows go build -o gradio

play: docker-db
	go build -o gradio
	./gradio -play

clean_data:
	@echo "Cleaning mp3s <1MB in ./recordings and ./split_music..."
	@find ./recordings ./split_music -type f -name "*.mp3" -size -1M -print -exec sh -c 'dir=$$(dirname "$$1"); rm -f "$$1"; if [ -z "$$(ls -A "$$dir" 2>/dev/null)" ]; then echo "Removing empty directory: $$dir"; rmdir "$$dir" 2>/dev/null || true; fi' _ {} \; 2>/dev/null || true
	@find ./recordings ./split_music -mindepth 1 -type d -empty -delete 2>/dev/null || true
	@echo "clean_data complete."

test:
	@$(MAKE) docker-db COCKROACH_PORT=26257
	@echo "Running tests..."
	go test ./... -p 1 -count=1 -v
