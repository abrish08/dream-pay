// Package contracts is the published language of the system: every message
// that crosses a module boundary is defined here and nowhere else.
//
// Rules:
//   - Modules import contracts and platform packages only, never another module.
//   - One sub-package per owning domain (merchants, payments, ...). The owning
//     module handles its requests and publishes its events; anyone may send
//     those requests or subscribe to those events.
//   - Contracts hold only DTOs, requests, events, error codes and constants.
//     No domain entities, database models or logic.
//   - Change contracts additively. Breaking changes get a new route or topic
//     version (e.g. "payment.succeeded.v2") while the old one is still served.
//
// When a module becomes its own service, nothing here changes: the same
// messages simply travel over NATS instead of in memory.
package contracts
