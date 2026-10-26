<template>
  <dl
    class="space-y-1 rounded-lg border border-gray-200 bg-gray-50 p-3 text-xs dark:border-dark-600 dark:bg-dark-800"
    data-testid="upstream-evidence"
  >
    <div class="flex gap-2"><dt class="shrink-0 text-gray-500 dark:text-gray-400">HTTP</dt><dd class="font-mono">{{ evidence.status }}</dd></div>
    <div v-if="evidence.model" class="flex gap-2"><dt class="shrink-0 text-gray-500 dark:text-gray-400">Model</dt><dd class="font-mono">{{ evidence.model }}</dd></div>
    <div v-if="evidence.requestId" class="flex gap-2 break-all"><dt class="shrink-0 text-gray-500 dark:text-gray-400">Request ID</dt><dd class="font-mono">{{ evidence.requestId }}</dd></div>
    <div v-if="evidence.errorCode" class="flex gap-2 break-all"><dt class="shrink-0 text-gray-500 dark:text-gray-400">Error code</dt><dd class="font-mono">{{ evidence.errorCode }}</dd></div>
    <div v-if="evidence.errorMessage" class="flex gap-2 break-all"><dt class="shrink-0 text-gray-500 dark:text-gray-400">Message</dt><dd>{{ evidence.errorMessage }}</dd></div>
  </dl>
</template>

<script setup lang="ts">
import type { AccountTestUpstreamEvidence } from '@/types/accountTestEvidence'

defineProps<{ evidence: AccountTestUpstreamEvidence }>()
</script>
