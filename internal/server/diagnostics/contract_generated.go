// Code generated from contract/v1/contract.schema.json; DO NOT EDIT.
package diagnostics

const ContractName = "axonhub.remote-diagnostics"
const ContractMajor = 1
const ContractMinor = 0
const ContractMediaType = "application/vnd.axonhub.diagnostics+json;version=1.0"
const SchemaSHA256 = "1fbfb245d2a20d2bf3de48c6bf1dd4a8ee53c0ddeba689c67965c6c2d0efff11"
const ContractMinorMinimum = 0
const ContractSubjectUserIDMinimum = 1
const ContractDefaultMaxRequests = 100
const ContractMaximumMaxRequests = 500
const ContractDefaultMaxExecutions = 500
const ContractMaximumMaxExecutions = 2000
const ContractDefaultMaxRelatedRecords = 1000
const ContractMaximumMaxRelatedRecords = 5000
const ContractDefaultMaxResponseBytes = 33554432
const ContractMinimumMaxResponseBytes = 1048576
const ContractMaximumMaxResponseBytes = 67108864

var ContractSectionNames = []string{"health", "configuration", "requests", "executions", "usage", "traces", "threads", "channels", "apiKeys", "accessGroups"}
