# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.2](https://github.com/MattermostFederal/mattermost-plugin-request-hub/compare/v0.6.1...v0.6.2) (2026-10-23)


### Bug Fixes

* **ci:** bump Node to 20.18 and run Playwright on PRs ([692c85c](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/692c85c53ef54afded01c902aac7365da2a8a953))
* **ci:** bump Node to 20.18 and run Playwright on PRs ([58ac949](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/58ac9496bfa915d8d18c4df3c3699eec0da3d6ca))
* **ci:** bump Node to 20.19 for rolldown native bindings ([145dca7](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/145dca791695033ce4f873e98b57c159b9ea8b7a))

## [0.6.1](https://github.com/MattermostFederal/mattermost-plugin-request-hub/compare/v0.6.0...v0.6.1) (2026-10-23)


### Bug Fixes

* **webapp:** clear eslint and tsc so release check-style passes ([5ea2c2b](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/5ea2c2b32005fd8c05582d465c90d2cb5eac9a4b))
* **webapp:** clear eslint/tsc and gate PRs on webapp lint ([1154d55](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/1154d55e6854580709c7f471b07b1c44924f6117))

## [0.6.0](https://github.com/MattermostFederal/mattermost-plugin-request-hub/compare/v0.5.2...v0.6.0) (2026-10-21)


### Features

* add sidebar category placement and compact resolved approval cards ([ee6774d](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/ee6774da98777b51febaaea1bd50df4634f34227))
* add two-step bot/webhook approvals, per-type toggles, and PR45 hardening ([039c06f](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/039c06fa84a824e34caf805c5ac3987b34a2447e))
* channel-admin request + tab-complete pickers + config cleanup ([8c0bb82](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8c0bb827cc7ea888714efadabe43057eef996a12))
* channel-admin requests (menu + Members-panel button) and orphaned-channel guard ([#15](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/15)) ([fc6e85e](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/fc6e85e2370c0547d561225ed5586d18f0a40e49))
* click-ops system console + prefix editor + member picker ([672f0e3](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/672f0e312cbe9a0d6f030a57ebf4952a159bd27b))
* color-code audit channel posts like approval outcomes ([1c48df9](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/1c48df92fc429e8b2fecd47991ff78ddb3b7a6aa))
* complete Request Hub with membership/guest flows and security hardening ([2154f60](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/2154f600b1cf47e02d882426d3ae5c8cb09bef18))
* expand plugin to full request hub with team, bot, and webhook request types ([8dc1b80](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8dc1b8092277994402381f6142efc014ce41c234))
* link audit log entries back to the approval ticket thread ([0131464](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/0131464a6300297430e2b16a8cc2af99c4010bfc))
* Mattermost Request Hub — membership/guest, docs, and security hardening ([429fe20](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/429fe20cbc2c3207d8ff6a88d7e1c9796254c172))
* sync two-step channel access and restructure request docs ([797b224](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/797b224ed854f63bb4c00bc569830125827dd06f))
* **webapp:** MemberPicker with avatars, badges, and MM-native sections ([f1c4b90](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/f1c4b90d79714051b7869ec2e00c101e82d72cfe))


### Bug Fixes

* address CodeRabbit and Copilot review findings on PR [#71](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/71) ([157038c](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/157038ce95326f3a93e048548e19bc067160b25d))
* address lint failures and Copilot review findings ([8397ab6](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8397ab6502eb2fa6d59bf25678d86badc5d542d8))
* address second-pass CodeRabbit and Copilot findings ([211554b](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/211554b2bddf637e9c1e327d6206c47d37090783))
* avoid double @ when formatting requester in audit lines ([8284a65](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8284a6571de622961bbb1fa7ce1141ad1751867c))
* block two-step self-approval and dead team-rail + ([becdf59](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/becdf59f7bf1e6d5e094bd795c3af59de3568075))
* **build:** build Go tools with the module's toolchain ([#51](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/51)) ([0849591](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/08495910158c08779f53102de550f8c348e79a60))
* **ci:** bump Go to 1.26.10 for GO-2026-6809 ([c4da99c](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c4da99ceaefc4cc54e631d64658e5251a28a0b93))
* **ci:** least-privilege secrets + correct SARIF ref on release call ([82c7f3a](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/82c7f3a3d9bb83cfeb54c816b54d2190ef29ac18))
* **ci:** publish release assets via reusable workflow ([56dc238](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/56dc238e407cea57d4e6c8fee4e7a527e70f430e))
* **ci:** publish release assets via reusable workflow ([0cc42d6](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/0cc42d6e5c1b3ba5ef82d658087ba63d25614080))
* correct stale fork references (alertmanager/template leftovers) ([c302569](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c3025691b71e42b68bb1d49cd246355c131bf71b))
* correct stale fork references (alertmanager/template leftovers) ([15cbe50](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/15cbe5017889485a80fd05ced2d72cc6b066595a))
* **dependabot:** drop include:scope on go/npm so bumps land in changelogs ([4f1cf8d](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/4f1cf8d72ee99c6f33a2161d9b6ac93f440b3e6b))
* **dependabot:** drop include:scope on go/npm to keep changelogs working ([818fda4](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/818fda40cc0148a652c08da7fa9dadcff0647d0b))
* **deps:** bump Go deps to clear HIGH CVEs blocking the Grype gate ([c2ca69e](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c2ca69e11e40ff59844baf545d63026e1e2c368a))
* **deps:** match the template's Go deps and Grype ignore ([c39aea7](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c39aea728597f24aa3546f8d5785b689542efe19))
* drop duplicate Approved/Denied line above audit cards ([2a04ac6](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/2a04ac61ec877c9bc8ab485dadbdf684a3abd44f))
* gofmt server/http.go for golangci-lint ([13d1f45](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/13d1f4531bb63b159fcd15ae3db09ba6f667d667))
* harden channel-request auth/validation and unbreak the webapp toolchain ([#20](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/20)) ([923647b](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/923647bae8a873a13dc00d8db2755d0d9febeb4b))
* keep claim cards pending when reload fails ([3cc33f9](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/3cc33f9431d063d62a2ebb40f99bb263192c4494))
* **lint:** resolve golangci-lint findings so the lint gate passes ([22c6a7b](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/22c6a7bd6d65281838b2aa71a38ca13844524284))
* loud two-step Approve blocks without breaking later clicks ([8c9142c](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8c9142cd680b3e09982347dcc3f4ab421139058c))
* preserve requester and ticket details when squashing approvals ([3e94481](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/3e94481ce606f65f859bc583b649fe63c06c18f9))
* remove unused userIsTwoStepChannelApprover for golangci-lint ([ee629ab](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/ee629ab2c8ecadfef46f1c2eb9a2fa2195587eaa))
* SearchUsers Limit=0 returns nothing, use explicit Limit=50 ([fc01236](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/fc012361c0c98a8b6a71d24b0991def0e4265cca))
* **webapp:** MemberPicker autocomplete now handles @-prefix + escapes modal clip ([0dd4367](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/0dd4367a7a6a80fe616cc64e9b0aabba8211f61d))
* **webapp:** pin @playwright/test to 1.62.1 to match experimental-ct-react ([#53](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/53)) ([9611c99](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/9611c99b7445748ecb74f422e2a2e64e280e554f))


### Dependencies

* **actions:** bump actions/download-artifact ([#56](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/56)) ([90ad88a](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/90ad88a4c7e7ca4de976c8a15b78294e51fed07a))
* **actions:** Bump actions/setup-go from 5.5.0 to 6.5.0 ([c38c197](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c38c19739a80ee323d8e9c20ea2d02d334bc8f4d))
* **actions:** bump actions/setup-go from 6.5.0 to 7.0.0 ([1f7b265](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/1f7b265bb59901d5cadb0b6523ddce5b94177523))
* **actions:** Bump actions/upload-artifact from 4.4.3 to 7.0.1 ([9edbc92](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/9edbc92a847dbf20ff282176a9aef43c446369f4))
* **actions:** Bump anchore/scan-action from 5.2.0 to 7.4.0 ([ff3c366](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/ff3c366a08570493c236530f99b63870e1345038))
* **actions:** Bump googleapis/release-please-action from 4.2.0 to 5.0.0 ([09304a2](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/09304a2ac0451209596466a5df85ab06c8363e45))
* **actions:** bump the actions-minor-patch group across 1 directory with 5 updates ([4031d52](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/4031d5216d87061e61269a59c4498631d5ac93e2))
* **actions:** bump the actions-minor-patch group across 1 directory with 7 updates ([#46](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/46)) ([ceb2f47](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/ceb2f476e57c884829044e074722221b90b7961a))
* **actions:** bump the actions-minor-patch group with 4 updates ([60a63e3](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/60a63e3e40b06f09cf13d8aaf90a8d21d329d8bb))
* **actions:** Bump the actions-minor-patch group with 5 updates ([0e28b27](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/0e28b278cea1f433970e63cec86ed8b8202919c8))
* **actions:** Bump the actions-minor-patch group with 5 updates ([8146156](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8146156e2a773ebeb5399f6f1454453d0c98563d))
* **go:** bump indirect Go modules to clear high-severity Grype findings ([#50](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/50)) ([bc04c64](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/bc04c64c6e49294f6ec0e50b6a66db8653bf6079))
* **go:** bump the go-minor-patch group across 1 directory with 2 updates ([#38](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/38)) ([59f2efc](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/59f2efcc6d33570f857d373455baa72c4698d490))
* **npm:** bump the npm-minor-patch group across 1 directory with 13 updates ([711f9ff](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/711f9ff949eb536774783d8b89c06b98674ae575))
* **npm:** bump the npm-minor-patch group across 1 directory with 13 updates ([#44](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/44)) ([0bd72be](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/0bd72be03d90d256b554f28388a4e0a22a20d21b))
* **npm:** Bump the npm-minor-patch group in /webapp with 2 updates ([cb41d0c](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/cb41d0ca964ea206bb8c0e784864cab70b60d9da))
* **npm:** bump the npm-minor-patch group in /webapp with 3 updates ([c32c27d](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c32c27dc655a593fc9095ccb8b4b407fe7d0dc02))
* **npm:** bump the npm-minor-patch group in /webapp with 5 updates ([#54](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/54)) ([f13f411](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/f13f411f9a3498f68acabb808da198b8d1e513aa))
* **npm:** Bump typescript from 6.0.3 to 7.0.2 in /webapp ([c132307](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c13230720b64a614afc1d04b8a5f43e0d4a14e5e))

## [0.3.0](https://github.com/MattermostFederal/mattermost-plugin-request-hub/compare/v0.2.1...v0.3.0) (2026-08-04)


### Features

* channel-admin requests (menu + Members-panel button) and orphaned-channel guard ([#15](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/15)) ([fc6e85e](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/fc6e85e2370c0547d561225ed5586d18f0a40e49))


### Bug Fixes

* **deps:** bump Go deps to clear HIGH CVEs blocking the Grype gate ([c2ca69e](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c2ca69e11e40ff59844baf545d63026e1e2c368a))
* harden channel-request auth/validation and unbreak the webapp toolchain ([#20](https://github.com/MattermostFederal/mattermost-plugin-request-hub/issues/20)) ([923647b](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/923647bae8a873a13dc00d8db2755d0d9febeb4b))


### Dependencies

* **actions:** bump actions/setup-go from 6.5.0 to 7.0.0 ([1f7b265](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/1f7b265bb59901d5cadb0b6523ddce5b94177523))
* **actions:** bump the actions-minor-patch group across 1 directory with 5 updates ([4031d52](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/4031d5216d87061e61269a59c4498631d5ac93e2))
* **actions:** Bump the actions-minor-patch group with 5 updates ([0e28b27](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/0e28b278cea1f433970e63cec86ed8b8202919c8))
* **npm:** bump the npm-minor-patch group across 1 directory with 13 updates ([711f9ff](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/711f9ff949eb536774783d8b89c06b98674ae575))
* **npm:** Bump the npm-minor-patch group in /webapp with 2 updates ([cb41d0c](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/cb41d0ca964ea206bb8c0e784864cab70b60d9da))
* **npm:** Bump typescript from 6.0.3 to 7.0.2 in /webapp ([c132307](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c13230720b64a614afc1d04b8a5f43e0d4a14e5e))

## [0.2.1](https://github.com/MattermostFederal/mattermost-plugin-request-hub/compare/v0.2.0...v0.2.1) (2026-07-06)


### Bug Fixes

* **dependabot:** drop include:scope on go/npm so bumps land in changelogs ([4f1cf8d](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/4f1cf8d72ee99c6f33a2161d9b6ac93f440b3e6b))

## [0.2.0](https://github.com/MattermostFederal/mattermost-plugin-request-hub/compare/v0.1.0...v0.2.0) (2026-07-06)


### Features

* channel-admin request + tab-complete pickers + config cleanup ([8c0bb82](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8c0bb827cc7ea888714efadabe43057eef996a12))
* click-ops system console + prefix editor + member picker ([672f0e3](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/672f0e312cbe9a0d6f030a57ebf4952a159bd27b))
* **webapp:** MemberPicker with avatars, badges, and MM-native sections ([f1c4b90](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/f1c4b90d79714051b7869ec2e00c101e82d72cfe))


### Bug Fixes

* **lint:** resolve golangci-lint findings so the lint gate passes ([22c6a7b](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/22c6a7bd6d65281838b2aa71a38ca13844524284))
* SearchUsers Limit=0 returns nothing, use explicit Limit=50 ([fc01236](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/fc012361c0c98a8b6a71d24b0991def0e4265cca))
* **webapp:** MemberPicker autocomplete now handles @-prefix + escapes modal clip ([0dd4367](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/0dd4367a7a6a80fe616cc64e9b0aabba8211f61d))


### Dependencies

* **actions:** Bump actions/setup-go from 5.5.0 to 6.5.0 ([c38c197](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/c38c19739a80ee323d8e9c20ea2d02d334bc8f4d))
* **actions:** Bump actions/upload-artifact from 4.4.3 to 7.0.1 ([9edbc92](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/9edbc92a847dbf20ff282176a9aef43c446369f4))
* **actions:** Bump anchore/scan-action from 5.2.0 to 7.4.0 ([ff3c366](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/ff3c366a08570493c236530f99b63870e1345038))
* **actions:** Bump googleapis/release-please-action from 4.2.0 to 5.0.0 ([09304a2](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/09304a2ac0451209596466a5df85ab06c8363e45))
* **actions:** Bump the actions-minor-patch group with 5 updates ([8146156](https://github.com/MattermostFederal/mattermost-plugin-request-hub/commit/8146156e2a773ebeb5399f6f1454453d0c98563d))

## [Unreleased]

### Added
- Initial plugin template.
