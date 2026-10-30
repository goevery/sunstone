# Changelog

## [2.0.0](https://github.com/goevery/sunstone/compare/v1.0.0...v2.0.0) (2026-10-30)


### ⚠ BREAKING CHANGES

* managed container names and metadata have changed. Existing deployments may require manual cleanup and redeployment.

### Features

* deploy HTTP workloads across VMs ([a3bc389](https://github.com/goevery/sunstone/commit/a3bc389897cc3a2ac8d1c00f515b60a0ab7dc187))
* inject workload secrets with Sunray ([8a5562b](https://github.com/goevery/sunstone/commit/8a5562bd92551744c316604fd50c6d8ae78c6928))
* operate deployed workloads ([1bbf7ae](https://github.com/goevery/sunstone/commit/1bbf7aebc971c33d9ee08d0a781f7d9ffb6898a0))
* persist SSH keys and renew OS Login authorization ([0609f70](https://github.com/goevery/sunstone/commit/0609f705f10adb22ad30154c62b57f2f698ebfa3))
* resolve workload secrets over SSH ([d7bb2c2](https://github.com/goevery/sunstone/commit/d7bb2c2e8f5a2d0c2df66c335aa407c578cd009a))


### Bug Fixes

* reuse ephemeral SSH credentials ([19f8cb7](https://github.com/goevery/sunstone/commit/19f8cb77268e836531adef928b85cecdcb3605e6))


### Performance Improvements

* overlap VM lookup with OS Login ([45d1e29](https://github.com/goevery/sunstone/commit/45d1e29625ae380c8614d6ea8b5bfb2f72ce7e9c))


### Code Refactoring

* organize workload operations around hosts and reusable sessions ([cdc48d7](https://github.com/goevery/sunstone/commit/cdc48d794deabf6c9c3431ff1ae7f52510815b02)), closes [#7](https://github.com/goevery/sunstone/issues/7)

## 1.0.0 (2026-09-29)


### Features

* add dependency documentation skills ([48abe24](https://github.com/goevery/sunstone/commit/48abe2437fd7654dc5842eaf5ef058d28fe13bb3))
* **cli:** scaffold suns commands ([4fec484](https://github.com/goevery/sunstone/commit/4fec4844010894ff897d796ee31f62ac5c6d8d2d))
* deploy background workloads ([#1](https://github.com/goevery/sunstone/issues/1)) ([07f1912](https://github.com/goevery/sunstone/commit/07f19129de219b030702106163eca541e8e10464))
* deploy background workloads sequentially ([#2](https://github.com/goevery/sunstone/issues/2)) ([4a2ab7f](https://github.com/goevery/sunstone/commit/4a2ab7fdfdb344db1b538b56f28188c67d010eb0))
* deploy HTTP workloads through Sunbeam ([#3](https://github.com/goevery/sunstone/issues/3)) ([e1ab812](https://github.com/goevery/sunstone/commit/e1ab8122e07570f8f0763231d3d088b744ecf024))


### Bug Fixes

* close IAP tunnel once ([#2](https://github.com/goevery/sunstone/issues/2)) ([766c2a5](https://github.com/goevery/sunstone/commit/766c2a5df07cf28846eb0e1bcefe8697543db1b5))
* impersonate deployment service account ([#1](https://github.com/goevery/sunstone/issues/1)) ([2fd8501](https://github.com/goevery/sunstone/commit/2fd8501951f9c56083daf962577d013837be1447))
