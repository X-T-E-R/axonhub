export interface RegistrationInvite {
  id: number;
  projectID: number;
  groupName: string;
  maxUses: number;
  usedUses: number;
  expiresAt: string | null;
  createdAt: string;
  status: 'valid' | 'used' | 'expired' | 'revoked' | 'unavailable';
}
export interface RegistrationGroup {
  id: number;
  name: string;
}
export interface RegistrationInviteInput {
  projectID: number;
  maxUses: number;
  expiresAt: string | null;
}
export const INVITE_FIELDS = 'id projectID groupName maxUses usedUses expiresAt createdAt status';
export const INVITES_QUERY = `query RegistrationInvites { registrationGroups { id name } registrationInvites { ${INVITE_FIELDS} } }`;
export const CREATE_INVITE = `mutation CreateRegistrationInvite($input:RegistrationInviteInput!){createRegistrationInvite(input:$input){code invite{${INVITE_FIELDS}}}}`;
export const REVOKE_INVITE = `mutation RevokeRegistrationInvite($id:Int!){revokeRegistrationInvite(id:$id){${INVITE_FIELDS}}}`;
