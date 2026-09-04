export const TemplateLiteralFixture = ({ condition }: { condition: boolean }) => (
  <div className={`pool-fixture-template-direct-missing ${condition ? 'pool-fixture-template-hole-a-missing' : 'pool-fixture-template-hole-b-missing'}`} />
);
