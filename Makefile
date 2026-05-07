build:
	go build -o migrate .
	mkdir -p input output logs
	@echo ""
	@echo "✓ Ready! Drop old projects into input/ then run:  ./migrate"
