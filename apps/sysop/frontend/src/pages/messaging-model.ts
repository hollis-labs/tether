// Control values stay canonical even when the readable label changes.
export function recipientOptions(
  agents: { urn: string; display_name?: string }[],
  aliases: { urn: string; alias: string }[],
) {
  const labels = new Map(aliases.map((a) => [a.urn, a.alias]))
  const options = new Map(
    agents.map((a) => [a.urn, { urn: a.urn, label: labels.get(a.urn) ?? a.display_name ?? a.urn }]),
  )
  for (const a of aliases) options.set(a.urn, { urn: a.urn, label: a.alias })
  return [...options.values()]
}
