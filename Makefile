run: build
	./gradio

build: 
	GOOS=windows go build -o gradio

play: build
	./gradio -play