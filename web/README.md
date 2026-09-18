# Frontend

Minimal static HTML/CSS/JS for the user management system.

## Pages

- `login.html` — POST `/api/login`, redirects to profile on success
- `signup.html` — POST `/api/signup`, redirects to login on success
- `profile.html` — GET `/api/user`, POST `/api/profile/nickname`, POST `/api/profile/avatar`, POST `/api/logout`

## Assets

- `style.css` — minimal shared styling
- `app.js` — placeholder for shared JS (empty for now)
- `default-avatar.png` — inline SVG placeholder shown when user has no uploaded avatar

## Notes

- All page scripts are inline for now; will be moved into `app.js` once shared logic (CSRF token, fetch wrapper) is needed.
- Forms use `fetch` with cookies (default `credentials: 'same-origin'`) — session cookie handling is server-side.
- File upload uses `multipart/form-data` via `FormData`.
- CSRF handling is TBD — will be added once the backend session/CSRF mechanism is in place (§4.9 of `system_design.md`).
- No build step. Serve statically from the HTTP server.

## Local preview (without backend)

Any static server works, e.g.:
```
python3 -m http.server 8080
```
Then open http://localhost:8080/login.html — forms will show but API calls will 404 until the HTTP server is wired up.
