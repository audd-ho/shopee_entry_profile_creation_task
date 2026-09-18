// TODO: HTTP route table.
//
// Endpoints (see §6 of system_design.md):
//   GET  /                        landing (login/signup UI, served from static)
//   POST /api/signup              handlers_auth.go:Signup
//   POST /api/login               handlers_auth.go:Login
//   POST /api/logout              handlers_auth.go:Logout        (auth required)
//   GET  /api/me                  handlers_auth.go:Me            (auth required)
//   POST /api/profile/nickname    handlers_profile.go:Nickname   (auth required)
//   POST /api/profile/avatar      handlers_profile.go:Avatar     (auth required)
//   GET  /uploads/*               static file serving of avatars
//   GET  /default.png             default avatar for users with avatar_version=0
//
// Static routes:
//   GET  /login.html, /signup.html, /profile.html, /style.css, /app.js
//     served from the frontend directory
package httpserver
