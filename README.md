## LSP proxy for multi language server

### Build
```
go build -o eglot-lspx .
```

### Usage
```
./eglot-lspx \
--provider hover=vscode-html-language-server \
--provider definition=vscode-html-language-server \
--provider completion=0,tailwindcss-language-server \
-- vscode-html-language-server --stdio \
-- tailwindcss-language-server --stdio
```

### How to config Eglot?
```
(defvar eglot-lspx-command (executable-find "eglot-lspx"))

(add-to-list 'eglot-server-programs
                  `((web-mode :language-id "html") . (,eglot-lspx-command
                                                      "--provider" "completion=0,tailwindcss-language-server"
                                                      "--" "vscode-html-language-server" "--stdio"
                                                      "--" "tailwindcss-language-server" "--stdio")))
```
