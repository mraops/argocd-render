.PHONY: encrypt decrypt

# Общая функция для обработки файлов/папок
# Файлы определяются по path_regex из .sops.yaml
define run_sops
	@sops_file=".sops.yaml"; \
	if [ ! -f "$$sops_file" ]; then \
		echo "Error: $$sops_file not found"; \
		exit 1; \
	fi; \
	patterns=$$(yq e '.creation_rules[].path_regex' "$$sops_file" 2>/dev/null); \
	if [ -z "$$patterns" ]; then \
		echo "Error: no path_regex found in $$sops_file"; \
		exit 1; \
	fi; \
	combined=$$(echo "$$patterns" | tr '\n' '|' | sed 's/|$$//'); \
	check_encrypted() { \
		case "$$1" in \
			*.yml|*.yaml) yq e 'has("sops")' "$$1" 2>/dev/null || echo "false" ;; \
			*.json) jq 'has("sops")' "$$1" 2>/dev/null || echo "false" ;; \
			*) grep -qE '"sops"|^sops_|^sops:' "$$1" 2>/dev/null && echo "true" || echo "false" ;; \
		esac; \
	}; \
	target="$(1)"; \
	if [ -z "$$target" ]; then \
		target="."; \
	fi; \
	if [ -d "$$target" ]; then \
		echo "$(2) files matching .sops.yaml rules"; \
		for file in $$(find "$$target" -type f -not -path "*/.git/*"); do \
			rel=$$(echo "$$file" | sed 's|^\./||'); \
			if ! echo "$$rel" | grep -qE "$$combined"; then \
				continue; \
			fi; \
			is_enc=$$(check_encrypted "$$file"); \
			if [ "$(2)" = "Encrypting" ]; then \
				if echo "$$is_enc" | grep -q true; then \
					echo "File $$file is already encrypted, skipping"; \
				else \
					echo "$(2) $$file"; \
					sops -$(3) -i "$$file"; \
				fi; \
			else \
				if echo "$$is_enc" | grep -q true; then \
					echo "$(2) $$file"; \
					sops -$(3) -i "$$file"; \
				else \
					echo "File $$file is not encrypted, skipping"; \
				fi; \
			fi; \
		done; \
	elif [ -f "$$target" ]; then \
		is_enc=$$(check_encrypted "$$target"); \
		if [ "$(2)" = "Encrypting" ]; then \
			if echo "$$is_enc" | grep -q true; then \
				echo "File $$target is already encrypted, skipping"; \
			else \
				echo "$(2) file $$target"; \
				sops -$(3) -i "$$target"; \
			fi; \
		else \
			if echo "$$is_enc" | grep -q true; then \
				echo "$(2) file $$target"; \
				sops -$(3) -i "$$target"; \
			else \
				echo "File $$target is not encrypted, skipping"; \
			fi; \
		fi; \
	else \
		echo "Error: $$target is not a valid file or folder"; \
		exit 1; \
	fi
endef

encrypt:
	$(call run_sops,$(word 2,$(MAKECMDGOALS)),"Encrypting","e")

decrypt:
	$(call run_sops,$(word 2,$(MAKECMDGOALS)),"Decrypting","d")

%:
	@:
