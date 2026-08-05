## 0.1.0 (unreleased)

FEATURES:

* `plugin` mode: extract everything the terraform plugin protocol (v5/v6) exposes from any provider binary — resources, data sources, ephemeral + list resources, actions, provider functions, resource identity schemas, server capabilities
* `src` mode: extract the full SDK-level schema from a provider source checkout — ForceNew, defaults, validators, ConflictsWith/ExactlyOneOf/AtLeastOneOf/RequiredWith, diff suppression, timeouts, state upgraders, and source file:line for every resource function
* `src --ssa`: static call-site analysis that recovers the literal arguments inside validator/default constructors (`IntBetween(1, 100)`, `StringInSlice([...], false)`) plus per-attribute declaration file:line
