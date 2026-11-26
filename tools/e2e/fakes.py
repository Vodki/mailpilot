# Fake Telegram (:8091) and Todoist (:8092) APIs for the end-to-end tests.
import http.server, json, threading, time

class TG(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        self.rfile.read(n)
        m = self.path.rsplit('/', 1)[-1]
        if m == 'getUpdates':
            time.sleep(2)
            r = []
        elif m == 'sendMessage':
            r = {"message_id": int(time.time() * 1000) % 10**9, "chat": {"id": 4242, "type": "private"}}
        elif m == 'getMe':
            r = {"id": 1, "is_bot": True, "first_name": "b", "username": "b"}
        else:
            r = True
        self.reply({"ok": True, "result": r})
    do_GET = do_POST
    def reply(self, o):
        b = json.dumps(o).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a):
        pass

class TD(TG):
    def do_GET(self):
        self.reply({"id": "u", "full_name": "Louis"})
    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        self.rfile.read(n)
        self.reply({"id": "t%d" % time.time_ns()})

threading.Thread(target=http.server.ThreadingHTTPServer(('127.0.0.1', 8092), TD).serve_forever, daemon=True).start()
http.server.ThreadingHTTPServer(('127.0.0.1', 8091), TG).serve_forever()
