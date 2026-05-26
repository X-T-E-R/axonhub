import React from 'react';

interface AxonHubIconProps {
  size?: number | string;
  className?: string;
  style?: React.CSSProperties;
}

export const AxonHubIcon: React.FC<AxonHubIconProps> = ({ size = 24, className, style }) => (
  <svg
    width={size}
    height={size}
    viewBox='0 0 24 24'
    fill='none'
    xmlns='http://www.w3.org/2000/svg'
    className={className}
    style={style}
    aria-hidden='true'
  >
    <rect x='3' y='3' width='18' height='18' rx='5' fill='currentColor' opacity='0.12' />
    <path d='M7 12.5h10M8.5 8.5h7M8.5 16.5h7' stroke='currentColor' strokeWidth='1.8' strokeLinecap='round' />
    <circle cx='7' cy='12.5' r='2' fill='currentColor' />
    <circle cx='17' cy='12.5' r='2' fill='currentColor' />
  </svg>
);

export default AxonHubIcon;
