// Package api implements the HTTP API layer for Rune.
// It exposes endpoints for search queries, service status, health checks,
// and WebSocket real-time event streaming.
//
// Endpoints:
//   GET /search?q=<query>&limit=<n>  — Ranked search results (JSON)
//   GET /status                        — Service state and statistics
//   GET /health                        — Health check (200/503)
//   WS  /events                        — Real-time file mutation events
package api
