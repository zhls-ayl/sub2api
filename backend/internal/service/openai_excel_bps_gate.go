package service

// isExcelBPSRouteCoolingDown covers the in-memory BPS cooldowns that the
// candidate pool does not filter: the account's BPS 429 cooldown and the
// (account, model) permission cooldown.
func (s *OpenAIGatewayService) isExcelBPSRouteCoolingDown(account *Account, requestedModel string) bool {
	return s.isExcelBPSModelCoolingDown(account, requestedModel) || s.isExcelBPSCoolingDown(account, requestedModel)
}
