#!/usr/bin/env python3
"""
Simple metrics server that returns CPU and memory usage percentages as JSON.
Useful for testing alternative metrics sources in RKE2 or other environments.

Usage:
    python3 metrics.py [port]

The server will listen on port 8080 by default (or specified port).
Access metrics at: http://localhost:8080/metrics
Health check at: http://localhost:8080/health

Endpoints:
    GET /metrics - Returns CPU and memory usage percentages
    GET /        - Same as /metrics (root endpoint)
    GET /health  - Returns health status

Example response:
{
    "cpu_usages": {
        "percentage": 75.5
    },
    "memory_usages": {
        "percentage": 68.2
    }
}
"""

import json
import http.server
import socketserver
import psutil
import sys
from urllib.parse import urlparse, parse_qs


class MetricsHandler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        parsed_path = urlparse(self.path)
        
        # Log all requests for debugging
        print(f"Received request: {self.path} from {self.client_address[0]}")
        
        # Handle /health endpoint
        if parsed_path.path == '/health':
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({"status": "healthy", "service": "metrics-server"}).encode())
            return
        
        # Serve both /metrics and root / for convenience
        if parsed_path.path not in ['/metrics', '/']:
            self.send_response(404)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({"error": "Not found", "path": parsed_path.path, "available": ["/metrics", "/", "/health"]}).encode())
            return
        
        try:
            # Get system CPU and memory usage
            cpu_percent = psutil.cpu_percent(interval=1)
            memory = psutil.virtual_memory()
            memory_percent = memory.percent
            
            # Create response JSON matching the expected format
            response = {
                "cpu_usages": {
                    "percentage": round(cpu_percent, 2)
                },
                "memory_usages": {
                    "percentage": round(memory_percent, 2)
                }
            }
            
            # Send response
            self.send_response(200)
            self.send_header('Content-type', 'application/json')
            self.send_header('Access-Control-Allow-Origin', '*')
            self.end_headers()
            response_json = json.dumps(response, indent=2)
            self.wfile.write(response_json.encode())
            print(f"Sent response: {response_json}")
            
        except Exception as e:
            self.send_response(500)
            self.send_header('Content-type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({"error": str(e)}).encode())
    
    def log_message(self, format, *args):
        # Custom log format
        sys.stderr.write("%s - - [%s] %s\n" %
                        (self.address_string(),
                         self.log_date_time_string(),
                         format % args))


def main():
    PORT = 8080
    
    if len(sys.argv) > 1:
        try:
            PORT = int(sys.argv[1])
        except ValueError:
            print(f"Invalid port number: {sys.argv[1]}")
            print("Usage: python3 metrics.py [port]")
            sys.exit(1)
    
    Handler = MetricsHandler
    
    # Allow reuse of address to avoid "Address already in use" errors
    socketserver.TCPServer.allow_reuse_address = True
    
    with socketserver.TCPServer(("0.0.0.0", PORT), Handler) as httpd:
        print(f"Metrics server running on http://0.0.0.0:{PORT}")
        print(f"Endpoints:")
        print(f"  - http://localhost:{PORT}/metrics (metrics data)")
        print(f"  - http://localhost:{PORT}/health (health check)")
        print(f"  - http://localhost:{PORT}/ (root, same as /metrics)")
        print(f"Container access: http://host.containers.internal:{PORT}/metrics")
        print("Press Ctrl+C to stop")
        print("Waiting for requests...")
        try:
            httpd.serve_forever()
        except KeyboardInterrupt:
            print("\nShutting down server...")
            httpd.shutdown()


if __name__ == "__main__":
    main()
