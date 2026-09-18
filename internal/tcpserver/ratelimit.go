// TODO: Rate limiting on login. SHELVED for v1.
//
// (See §4.8 of system_design.md — deferred; revisit if time permits after primary goals.)
//
// When implemented:
//   - Per-IP counter: rl:login:ip:<ip>  → INCR + EXPIRE 5min → block above 20 fails
//   - Per-user counter: rl:login:user:<uname>  → INCR + EXPIRE 15min → lock above 5 fails
//   - On successful login: DEL rl:login:user:<uname>
//   - Check counters BEFORE bcrypt to avoid CPU waste on brute-force attempts
package tcpserver
