
```
(setq eglot-add-on-modes
      '((web-mode
         :add-on (tailwindcss-mode)
         :completion (web-mode tailwindcss-mode)
         :diagnostics web-mode)))
         
(eglot-add-on-mode)
```